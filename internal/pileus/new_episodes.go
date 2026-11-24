package pileus

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// New episodes of followed series. Once a day each series some profile
// follows (in a list as a series, or watched lately) has its details asked
// to its plugin, in the background and one at a time; the latest episode the
// plugin reports (SeriesDetails.latest_episode) goes to series_latest with
// the time the server first saw it. Nothing here knows any site: a plugin
// that doesn't report latest_episode simply never flags anything.
//
// A profile gets "new_episode" on a series when that episode came out after
// it last watched the series (or added it, if never watched), is past where
// it got to, and was seen in the last newEpisodeWindow.

const (
	newEpisodeWindow   = 14 * 24 * time.Hour
	newEpisodeRecheck  = 20 * time.Hour // a series is checked at most this often
	newEpisodeTick     = time.Hour      // how often the checker looks for due series
	newEpisodeFirstRun = 10 * time.Minute
	newEpisodeActive   = 90 // days: a series watched since then counts as followed
	newEpisodePerTick  = 200
	newEpisodeCallWait = 5 * time.Second // for a free Lua state; never queues behind clients
	newEpisodeTimeout  = 60 * time.Second
	BadgeNewEpisode    = "new_episode"
)

var newEpisodeOnce sync.Once

// startNewEpisodeChecker starts the background check (once per process).
func startNewEpisodeChecker() {
	newEpisodeOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[new-episodes] PANIC recuperato: %v", r)
				}
			}()
			time.Sleep(newEpisodeFirstRun)
			for {
				checkFollowedSeries(context.Background())
				time.Sleep(newEpisodeTick)
			}
		}()
	})
}

// checkFollowedSeries checks the followed series not checked lately.
func checkFollowedSeries(ctx context.Context) {
	if managers.DB == nil {
		return
	}
	due, err := managers.DB.FollowedSeries(newEpisodeActive, time.Now().Add(-newEpisodeRecheck).Unix())
	if err != nil {
		log.Printf("[new-episodes] serie seguite: %v", err)
		return
	}
	if len(due) > newEpisodePerTick {
		due = due[:newEpisodePerTick]
	}
	pause := time.Second
	if core.LowEnd() {
		pause = 3 * time.Second
	}
	found := 0
	for _, k := range due {
		if ctx.Err() != nil {
			return
		}
		if checkSeries(ctx, k[0], k[1]) {
			found++
		}
		time.Sleep(pause)
	}
	if len(due) > 0 {
		log.Printf("[new-episodes] %d serie controllate, %d con un episodio nuovo", len(due), found)
	}
}

// checkSeries asks pluginID for seriesID's latest episode and stores it;
// true when it is a newer one than stored. A plugin that isn't running is
// skipped (checked again next tick); any other failure counts as a check.
func checkSeries(ctx context.Context, pluginID, seriesID string) bool {
	if !engine.LuaPlugins.Has(pluginID) || !engine.LuaPlugins.IsOperational(pluginID) {
		return false
	}
	le := fetchLatestEpisode(ctx, pluginID, seriesID)
	var s, e int32
	var air string
	if le != nil {
		s, e, air = le.SeasonNumber, le.EpisodeNumber, le.AirDate
	}
	newer, err := managers.DB.SetSeriesLatest(pluginID, seriesID, s, e, air)
	if err != nil {
		log.Printf("[new-episodes] %s/%s: %v", pluginID, seriesID, err)
	}
	return newer
}

// fetchLatestEpisode: the plugin's details of a series, background class.
func fetchLatestEpisode(ctx context.Context, pluginID, seriesID string) *gen.LatestEpisode {
	cctx, cancel := context.WithTimeout(ctx, newEpisodeTimeout)
	defer cancel()
	b, err := engine.LuaPlugins.CallEntrypointJSONBackgroundCtx(cctx, pluginID, engine.EPGetDetails,
		map[string]any{"media_id": seriesID}, "", nil, newEpisodeCallWait)
	if err != nil || isNullJSON(b) {
		return nil
	}
	var lm luaMeta
	if json.Unmarshal(b, &lm) != nil {
		return nil
	}
	return lm.LatestEpisode.proto()
}

// newEpisodesFor lists owner's series with a new episode (see the file
// comment), most recent first.
func newEpisodesFor(owner string, now time.Time) ([]*gen.NewEpisode, error) {
	latest, err := managers.DB.AllSeriesLatest()
	if err != nil || len(latest) == 0 {
		return nil, err
	}
	watched, err := managers.DB.SeriesWatchState(owner)
	if err != nil {
		return nil, err
	}
	list, err := managers.DB.GetMyList(owner, "")
	if err != nil {
		return nil, err
	}
	type follow struct {
		since         int64
		season, ep    int32
		title, poster string
	}
	follows := map[[2]string]follow{}
	for _, e := range list {
		if e.MediaType == "series" {
			follows[[2]string{e.PluginID, e.MediaID}] = follow{since: e.AddedAt, title: e.Title, poster: e.PosterURL}
		}
	}
	for k, w := range watched {
		f := follows[k]
		// Watched: news is what came after the last time it was watched.
		f.since, f.season, f.ep = w.LastWatched, w.Season, w.Episode
		if f.title == "" {
			f.title = w.Title
		}
		if f.poster == "" {
			f.poster = w.Poster
		}
		follows[k] = f
	}
	cutoff := now.Add(-newEpisodeWindow).Unix()
	var out []*gen.NewEpisode
	for k, f := range follows {
		l, ok := latest[k]
		if !ok || l.FoundAt == 0 || l.FoundAt < cutoff || l.FoundAt <= f.since {
			continue
		}
		if !managers.EpisodeAfter(l.Season, l.Episode, f.season, f.ep) {
			continue
		}
		out = append(out, &gen.NewEpisode{
			PluginId: k[0], SeriesId: k[1], Title: f.title, PosterUrl: f.poster,
			SeasonNumber: l.Season, EpisodeNumber: l.Episode, AirDate: l.AirDate, FoundAt: l.FoundAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FoundAt > out[j].FoundAt })
	return out, nil
}

// newEpisodeSet: the keys of newEpisodesFor, for badging lists. Errors just
// mean no badges.
func newEpisodeSet(owner string) map[[2]string]bool {
	items, err := newEpisodesFor(owner, time.Now())
	if err != nil {
		log.Printf("[new-episodes] %v", err)
	}
	set := make(map[[2]string]bool, len(items))
	for _, it := range items {
		set[[2]string{it.PluginId, it.SeriesId}] = true
	}
	return set
}

func (h *MediaHandler) GetNewEpisodes(ctx context.Context, _ *gen.NewEpisodesRequest) (*gen.NewEpisodesResponse, error) {
	owner, err := myListOwner(ctx)
	if err != nil {
		return nil, err
	}
	items, err := newEpisodesFor(owner, time.Now())
	if err != nil {
		log.Printf("[pileus/media] GetNewEpisodes: %v", err)
		return nil, status.Error(codes.Internal, "lettura dei nuovi episodi non riuscita")
	}
	return &gen.NewEpisodesResponse{Items: items}, nil
}
