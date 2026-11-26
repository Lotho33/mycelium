package pileus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// Release calendar (GetCalendar). Plugins with a `calendar` entrypoint say
// what comes out between two dates; the server asks them in parallel, keeps
// each answer for calendarTTL, merges them by release time and marks the
// series the profile follows. Nothing here knows a site: a date and an
// "available" flag are all it reads.
//
// The plugin gets args {from, to, follows}: the dates ("YYYY-MM-DD",
// inclusive) and the ids of the series this profile follows in that plugin —
// a hint to keep the work small (e.g. look up only those), never a filter
// the server relies on.

const (
	calendarTTL         = 3 * time.Hour
	calendarFailTTL     = 5 * time.Minute // a failed plugin is asked again sooner
	calendarCallTimeout = 30 * time.Second
	calendarDefaultDays = 7
	calendarMaxDays     = 31
	calendarMaxEntries  = 2000 // per plugin answer
	calendarCacheMax    = 256
)

type calendarCacheEntry struct {
	entries []*gen.CalendarEntry
	failed  bool
	expires time.Time
}

var (
	calendarMu    sync.Mutex
	calendarCache = map[string]calendarCacheEntry{}
	calendarNow   = time.Now // test seam
)

// calendarRange: the request's dates, defaulted and bounded.
func calendarRange(from, to string) (time.Time, time.Time, error) {
	loc := time.Local
	var f, t time.Time
	var err error
	if strings.TrimSpace(from) == "" {
		n := calendarNow().In(loc)
		f = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	} else if f, err = time.ParseInLocation("2006-01-02", strings.TrimSpace(from), loc); err != nil {
		return f, t, status.Error(codes.InvalidArgument, "from: data non valida (YYYY-MM-DD)")
	}
	if strings.TrimSpace(to) == "" {
		t = f.AddDate(0, 0, calendarDefaultDays-1)
	} else if t, err = time.ParseInLocation("2006-01-02", strings.TrimSpace(to), loc); err != nil {
		return f, t, status.Error(codes.InvalidArgument, "to: data non valida (YYYY-MM-DD)")
	}
	if t.Before(f) {
		return f, t, status.Error(codes.InvalidArgument, "to precede from")
	}
	if t.Sub(f) > (calendarMaxDays-1)*24*time.Hour+time.Hour {
		t = f.AddDate(0, 0, calendarMaxDays-1)
	}
	return f, t, nil
}

// parseReleaseAt reads a plugin's release_at: "YYYY-MM-DD" (all day), or a
// date and time with or without seconds and offset (no offset = the
// server's time zone).
func parseReleaseAt(s string) (time.Time, bool, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, false
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, true, true
	}
	s = strings.Replace(s, " ", "T", 1)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, false, true
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, false, true
		}
	}
	return time.Time{}, false, false
}

// calendarItem is one plugin entry: a catalog item plus its release.
type calendarItem struct {
	luaMeta
	ReleaseAt string `json:"release_at"`
	Available bool   `json:"available"`
	SeriesID  string `json:"series_id"`
}

// decodeCalendar turns a plugin answer (array, or {items = …}) into entries
// within [from, to]; entries without a readable date are dropped.
func decodeCalendar(pluginID string, b json.RawMessage, from, to time.Time) ([]*gen.CalendarEntry, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		var w struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(b, &w) != nil {
			if isNullJSON(b) || strings.TrimSpace(string(b)) == "{}" {
				return nil, nil
			}
			return nil, err
		}
		raw = w.Items
	}
	end := to.AddDate(0, 0, 1)
	out := make([]*gen.CalendarEntry, 0, len(raw))
	for _, r := range raw {
		if len(out) >= calendarMaxEntries {
			break
		}
		var ci calendarItem
		if json.Unmarshal(r, &ci) != nil || ci.ID == "" {
			continue
		}
		at, allDay, ok := parseReleaseAt(ci.ReleaseAt)
		if !ok || at.Before(from) || !at.Before(end) {
			continue
		}
		series := ci.SeriesID
		if series == "" {
			series = ci.ShowID
		}
		out = append(out, &gen.CalendarEntry{
			PluginId:  pluginID,
			Item:      luaMetaToCatalogItem(ci.luaMeta),
			ReleaseAt: at.Format(time.RFC3339),
			AllDay:    allDay,
			Available: ci.Available,
			SeriesId:  series,
		})
	}
	return out, nil
}

// calendarPlugins: the running plugins with a calendar entrypoint (or just
// only, when set).
func calendarPlugins(only string) []string {
	var ids []string
	for _, mf := range engine.LuaPlugins.GetMeta() {
		if only != "" && mf.ID != only {
			continue
		}
		if mf.Entrypoints[engine.EPCalendar] == "" || !engine.LuaPlugins.Has(mf.ID) || !engine.LuaPlugins.IsOperational(mf.ID) {
			continue
		}
		ids = append(ids, mf.ID)
	}
	sort.Strings(ids)
	return ids
}

// calendarPluginVersion: the running version of pluginID ("" if unknown).
func calendarPluginVersion(pluginID string) string {
	for _, mf := range engine.LuaPlugins.GetMeta() {
		if mf.ID == pluginID {
			return mf.Version
		}
	}
	return ""
}

// pluginCalendar asks one plugin (or its cached answer).
func pluginCalendar(ctx context.Context, pluginID string, from, to time.Time, follows []string, profileID string) ([]*gen.CalendarEntry, bool) {
	sort.Strings(follows)
	sum := sha256.Sum256([]byte(strings.Join(follows, "\x00")))
	// The version is in the key: an updated plugin is asked again at once
	// instead of serving the old version's answer for calendarTTL.
	key := pluginID + "@" + calendarPluginVersion(pluginID) + "|" + from.Format("2006-01-02") + "|" + to.Format("2006-01-02") + "|" + hex.EncodeToString(sum[:8])
	now := calendarNow()
	calendarMu.Lock()
	if c, ok := calendarCache[key]; ok && now.Before(c.expires) {
		calendarMu.Unlock()
		return c.entries, !c.failed
	}
	calendarMu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, calendarCallTimeout)
	defer cancel()
	luaFollows := make([]any, len(follows)) // the Lua bridge takes []any, not []string
	for i, f := range follows {
		luaFollows[i] = f
	}
	b, err := engine.LuaPlugins.CallEntrypointJSONCtx(cctx, pluginID, engine.EPCalendar, map[string]any{
		"from":    from.Format("2006-01-02"),
		"to":      to.Format("2006-01-02"),
		"follows": luaFollows,
	}, profileID)
	var entries []*gen.CalendarEntry
	if err == nil {
		entries, err = decodeCalendar(pluginID, b, from, to)
	}
	if err != nil {
		log.Printf("[calendar] %s: %v", pluginID, err)
	}
	ttl := calendarTTL
	if err != nil {
		ttl = calendarFailTTL
	}
	calendarMu.Lock()
	if len(calendarCache) >= calendarCacheMax {
		for k, c := range calendarCache {
			if !now.Before(c.expires) {
				delete(calendarCache, k)
			}
		}
		if len(calendarCache) >= calendarCacheMax {
			calendarCache = map[string]calendarCacheEntry{}
		}
	}
	calendarCache[key] = calendarCacheEntry{entries: entries, failed: err != nil, expires: now.Add(ttl)}
	calendarMu.Unlock()
	return entries, err == nil
}

// followedSeries: the series owner follows (list or history), by plugin.
func followedSeries(owner string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	add := func(p, s string) {
		if out[p] == nil {
			out[p] = map[string]bool{}
		}
		out[p][s] = true
	}
	if list, err := managers.DB.GetMyList(owner, ""); err == nil {
		for _, e := range list {
			if e.MediaType == "series" {
				add(e.PluginID, e.MediaID)
			}
		}
	}
	if ws, err := managers.DB.SeriesWatchState(owner); err == nil {
		for k := range ws {
			add(k[0], k[1])
		}
	}
	return out
}

func (h *MediaHandler) GetCalendar(ctx context.Context, req *gen.CalendarRequest) (*gen.CalendarResponse, error) {
	owner, err := myListOwner(ctx)
	if err != nil {
		return nil, err
	}
	from, to, err := calendarRange(req.GetFrom(), req.GetTo())
	if err != nil {
		return nil, err
	}
	follows := followedSeries(owner)
	plugins := calendarPlugins(req.GetPluginId())
	profileID := profileIDFromCtx(ctx)

	type answer struct {
		id      string
		entries []*gen.CalendarEntry
		ok      bool
	}
	answers := make([]answer, len(plugins))
	var wg sync.WaitGroup
	for i, id := range plugins {
		var ids []string
		for s := range follows[id] {
			ids = append(ids, s)
		}
		wg.Add(1)
		go func(i int, id string, ids []string) {
			defer wg.Done()
			e, ok := pluginCalendar(ctx, id, from, to, ids, profileID)
			answers[i] = answer{id, e, ok}
		}(i, id, ids)
	}
	wg.Wait()

	resp := &gen.CalendarResponse{}
	for _, a := range answers {
		if !a.ok {
			resp.FailedPlugins = append(resp.FailedPlugins, a.id)
		}
		for _, e := range a.entries {
			series := e.SeriesId
			if series == "" && e.Item.GetMediaType() == "series" {
				series = e.Item.GetId()
			}
			// Cached entries are shared: copy before marking.
			c := &gen.CalendarEntry{
				PluginId: e.PluginId, Item: e.Item, ReleaseAt: e.ReleaseAt, AllDay: e.AllDay,
				Available: e.Available, SeriesId: e.SeriesId,
				Followed: series != "" && follows[e.PluginId][series],
			}
			if req.GetFollowedOnly() && !c.Followed {
				continue
			}
			resp.Entries = append(resp.Entries, c)
		}
	}
	sort.SliceStable(resp.Entries, func(i, j int) bool {
		a, b := resp.Entries[i], resp.Entries[j]
		if a.ReleaseAt != b.ReleaseAt {
			ta, _ := time.Parse(time.RFC3339, a.ReleaseAt)
			tb, _ := time.Parse(time.RFC3339, b.ReleaseAt)
			if !ta.Equal(tb) {
				return ta.Before(tb)
			}
		}
		return a.AllDay && !b.AllDay // a timed release after the day's undated ones
	})
	return resp, nil
}
