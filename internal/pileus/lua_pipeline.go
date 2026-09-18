package pileus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"mycelium/internal/engine"
	"mycelium/pkg/models"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// debugLog controls verbose logging of raw plugin responses.
// Enable with MYCELIUM_DEBUG=1. Off by default to avoid leaking credentials
// that plugins may embed in metadata/headers.
var debugLog = os.Getenv("MYCELIUM_DEBUG") == "1"

// luaPipelineProvider adapts the embedded Lua plugin engine to PipelineProvider.
type luaPipelineProvider struct{ id string }

func (p *luaPipelineProvider) GetCatalog(ctx context.Context, catalogID string, page int) ([]*gen.CatalogItem, bool, error) {
	lms, err := luaCallLuaMetas(p.id, engine.EPGetCatalog, map[string]any{
		"catalog_id": catalogID,
		"page":       page,
	}, profileIDFromCtx(ctx))
	if err != nil {
		return nil, false, err
	}
	return luaMetasToCatalogItems(lms), len(lms) > 0, nil
}

func (p *luaPipelineProvider) GetSearchFilters(ctx context.Context) ([]*gen.SearchFilter, error) {
	b, err := engine.LuaPlugins.CallEntrypointJSON(p.id, engine.EPGetSearchFilters, map[string]any{}, profileIDFromCtx(ctx))
	if err != nil || isNullJSON(b) {
		return nil, nil
	}
	var defs []luaFilterDef
	if err := json.Unmarshal(b, &defs); err != nil {
		return nil, nil
	}
	filters := make([]*gen.SearchFilter, 0, len(defs))
	for _, d := range defs {
		f := &gen.SearchFilter{Id: d.ID, Label: d.Label, Type: d.Type}
		for _, o := range d.Options {
			f.Options = append(f.Options, &gen.FilterOption{Id: o.ID, Label: o.Label})
		}
		filters = append(filters, f)
	}
	return filters, nil
}

func (p *luaPipelineProvider) Search(ctx context.Context, query string, page int, filters map[string]string) ([]*gen.CatalogItem, bool, error) {
	filtersAny := make(map[string]any, len(filters))
	for k, v := range filters {
		filtersAny[k] = v
	}
	if debugLog {
		log.Printf("[pileus/search] plugin=%s query=%q page=%d filters=%v", p.id, query, page, filters)
	}
	b, err := engine.LuaPlugins.CallEntrypointJSON(p.id, engine.EPSearch, map[string]any{
		"query":   query,
		"page":    page,
		"filters": filtersAny,
	}, profileIDFromCtx(ctx))
	if err != nil {
		return nil, false, err
	}
	lms, hasMore, err := luaMetasFromJSON(b)
	if err != nil {
		return nil, false, err
	}
	if debugLog {
		if len(b) < 300 {
			log.Printf("[pileus/search] raw=%s hasMore=%v items=%d", string(b), hasMore, len(lms))
		} else {
			log.Printf("[pileus/search] hasMore=%v items=%d raw_prefix=%s", hasMore, len(lms), string(b[:200]))
		}
	}
	return luaMetasToCatalogItems(lms), hasMore, nil
}

func (p *luaPipelineProvider) GetDetails(ctx context.Context, mediaID string) (*gen.DetailsResponse, error) {
	b, err := engine.LuaPlugins.CallEntrypointJSON(p.id, engine.EPGetDetails, map[string]any{
		"media_id": mediaID,
	}, profileIDFromCtx(ctx))
	if err != nil {
		return nil, err
	}
	if isNullJSON(b) {
		return nil, status.Errorf(codes.NotFound, "item not found: %s", mediaID)
	}
	if debugLog {
		log.Printf("[GetDetails] raw json for %s: %.500s", mediaID, string(b))
	}
	var lm luaMeta
	if err := json.Unmarshal(b, &lm); err != nil {
		return nil, err
	}
	if debugLog {
		log.Printf("[GetDetails] lm.MediaType=%q isDir=%v seasons=%d", lm.MediaType, lm.IsDir, len(lm.Seasons))
	}
	resp := &gen.DetailsResponse{Item: luaMetaToCatalogItem(lm)}
	populateDetails(resp, lm)
	return resp, nil
}

func (p *luaPipelineProvider) Browse(ctx context.Context, dirID string, page int) (*gen.BrowseResponse, error) {
	lms, err := luaCallLuaMetas(p.id, engine.EPBrowse, map[string]any{
		"directory_id": dirID,
		"page":         page,
	}, profileIDFromCtx(ctx))
	if err != nil {
		return nil, err
	}
	resp := &gen.BrowseResponse{HasMore: len(lms) > 0}
	for _, lm := range lms {
		if lm.MediaType == models.TypeEpisode {
			resp.Episodes = append(resp.Episodes, luaMetaToEpisodeInfo(lm))
		} else {
			resp.Items = append(resp.Items, luaMetaToCatalogItem(lm))
		}
	}
	return resp, nil
}

func (p *luaPipelineProvider) GetStreams(ctx context.Context, mediaID string) ([]*gen.StreamSource, error) {
	b, err := engine.LuaPlugins.CallEntrypointJSON(p.id, engine.EPGetStreams, map[string]any{
		"media_id": mediaID,
	}, profileIDFromCtx(ctx))
	if err != nil {
		return nil, err
	}

	type srcEntry struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Quality string `json:"quality"`
		IsLive  bool   `json:"is_live"`
	}
	toProto := func(ss []srcEntry) []*gen.StreamSource {
		out := make([]*gen.StreamSource, 0, len(ss))
		for _, s := range ss {
			out = append(out, &gen.StreamSource{Id: s.ID, Label: s.Label, Quality: s.Quality, IsLive: s.IsLive})
		}
		return out
	}
	var direct []srcEntry
	if json.Unmarshal(b, &direct) == nil && len(direct) > 0 {
		return toProto(direct), nil
	}
	var wrapped struct {
		Sources []srcEntry `json:"sources"`
	}
	if json.Unmarshal(b, &wrapped) == nil && len(wrapped.Sources) > 0 {
		return toProto(wrapped.Sources), nil
	}
	return []*gen.StreamSource{{Id: mediaID, Label: "Stream", Quality: "Auto"}}, nil
}

func (p *luaPipelineProvider) ResolveStream(ctx context.Context, streamID string, onProgress engine.ProgressFunc) (string, map[string]string, bool, map[string]string, error) {
	b, err := engine.LuaPlugins.CallEntrypointJSONWithProgress(p.id, engine.EPResolveStream, map[string]any{
		"stream_id": streamID,
	}, profileIDFromCtx(ctx), onProgress)
	if err != nil {
		return "", nil, false, nil, err
	}
	if isNullJSON(b) {
		return "", nil, false, nil, status.Error(codes.NotFound, "stream not resolved: plugin returned nil")
	}
	var result struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		IsLive  bool              `json:"is_live"`
		Extra   map[string]string `json:"extra"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return "", nil, false, nil, fmt.Errorf("invalid lua result: %w", err)
	}
	if result.URL == "" {
		return "", nil, false, nil, status.Error(codes.NotFound, "stream not resolved: empty URL")
	}
	return result.URL, result.Headers, result.IsLive, result.Extra, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// luaFilterDef — search filter shape returned by Lua plugins
// ─────────────────────────────────────────────────────────────────────────────

type luaFilterDef struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	Options []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"options"`
}

// ─────────────────────────────────────────────────────────────────────────────
// flexMap tolerates an empty JSON array ("[]") in place of an empty object.
// Lua serialises empty tables as arrays, so an empty metadata table becomes [].
// ─────────────────────────────────────────────────────────────────────────────

type flexMap map[string]string

func (m *flexMap) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '[' {
		*m = nil
		return nil
	}
	type plain map[string]string
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*m = flexMap(p)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// luaMeta — flat struct mirroring the JSON shape that Lua plugins emit.
// ─────────────────────────────────────────────────────────────────────────────

type luaMeta struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	MediaType     string   `json:"media_type"`
	PosterURL     string   `json:"poster_url"`
	FanartURL     string   `json:"fanart_url"`
	Plot          string   `json:"plot"`
	Year          int32    `json:"year"`
	Rating        float64  `json:"rating"`
	Genres        []string `json:"genres"`
	IsDir         bool     `json:"is_dir"`
	Category      string   `json:"category"`
	ParentID      string   `json:"parent_id"`
	ShowID        string   `json:"show_id"`
	SeasonNumber  int32    `json:"season_number"`
	EpisodeNumber int32    `json:"episode_number"`
	ProviderID    string   `json:"provider_id"`
	Metadata      flexMap  `json:"metadata"`
	Extra         flexMap  `json:"extra"`
	DirectStream  string   `json:"direct_stream_url"`
	IsExternal    bool     `json:"is_external"`
	LogoURL       string   `json:"logo_url"`
	BannerURL     string   `json:"banner_url"`

	// episode-specific
	ThumbnailURL string  `json:"thumbnail_url"`
	AirDate      string  `json:"air_date"`
	Duration     int32   `json:"duration"`
	Vote         float64 `json:"vote"`

	// season list for series details
	Seasons []struct {
		Number        int32  `json:"number"`
		Label         string `json:"label"`
		DirectoryID   string `json:"directory_id"`
		ID            string `json:"id"`
		PosterURL     string `json:"poster_url"`
		Year          int32  `json:"year"`
		EpisodeCount  int32  `json:"episode_count"`
		Overview      string `json:"overview"`
		SeasonAirDate string `json:"air_date"`
	} `json:"seasons"`

	// typed details
	Runtime       string   `json:"runtime"`
	Cast          []string `json:"cast"`
	Directors     []string `json:"directors"`
	TrailerURL    string   `json:"trailer_url"`
	Released      string   `json:"released"`
	Tagline       string   `json:"tagline"`
	ContentRating string   `json:"content_rating"`
	Status        string   `json:"status"`
	TotalSeasons  int32    `json:"total_seasons"`
	TotalEpisodes int32    `json:"total_episodes"`
	Network       string   `json:"network"`

	// live
	ChannelName   string   `json:"channel_name"`
	ChannelLogo   string   `json:"channel_logo"`
	StreamStart   string   `json:"stream_start"`
	StreamEnd     string   `json:"stream_end"`
	SportCategory string   `json:"sport_category"`
	Teams         []string `json:"teams"`
	Competition   string   `json:"competition"`
	IsReplay      bool     `json:"is_replay"`

	// video
	ChannelURL    string   `json:"channel_url"`
	PublishedAt   string   `json:"published_at"`
	ViewCount     int64    `json:"view_count"`
	LikeCount     int64    `json:"like_count"`
	VideoDuration string   `json:"duration_str"`
	Tags          []string `json:"tags"`

	// music
	Artist      string   `json:"artist"`
	Album       string   `json:"album"`
	AlbumArtURL string   `json:"album_art_url"`
	TrackNumber int32    `json:"track_number"`
	DiscNumber  int32    `json:"disc_number"`
	Composers   []string `json:"composers"`
	LyricsURL   string   `json:"lyrics_url"`

	// podcast
	ShowName string `json:"show_name"`
	Author   string `json:"author"`
	AudioURL string `json:"audio_url"`
}

// ─────────────────────────────────────────────────────────────────────────────
// populateDetails fills the typed oneof details field of resp from a luaMeta.
// ─────────────────────────────────────────────────────────────────────────────

func populateDetails(resp *gen.DetailsResponse, lm luaMeta) {
	if debugLog {
		log.Printf("[GetDetails] logoUrl=%q mediaType=%q", lm.LogoURL, lm.MediaType)
	}
	switch lm.MediaType {
	case models.TypeMovie:
		resp.Details = &gen.DetailsResponse_Movie{Movie: &gen.MovieDetails{
			Plot:          lm.Plot,
			Year:          lm.Year,
			Genres:        lm.Genres,
			Runtime:       lm.Runtime,
			Cast:          lm.Cast,
			Directors:     lm.Directors,
			TrailerUrl:    lm.TrailerURL,
			Released:      lm.Released,
			Tagline:       lm.Tagline,
			ContentRating: lm.ContentRating,
			PosterUrl:     lm.PosterURL,
			FanartUrl:     lm.FanartURL,
			LogoUrl:       lm.LogoURL,
		}}
	case models.TypeSeries:
		sd := &gen.SeriesDetails{
			Plot:           lm.Plot,
			Year:           lm.Year,
			Genres:         lm.Genres,
			Status:         lm.Status,
			ContentRating:  lm.ContentRating,
			Network:        lm.Network,
			EpisodeRuntime: lm.Runtime,
			Cast:           lm.Cast,
			Creators:       lm.Directors,
			TrailerUrl:     lm.TrailerURL,
			FirstAirDate:   lm.Released,
			PosterUrl:      lm.PosterURL,
			FanartUrl:      lm.FanartURL,
			LogoUrl:        lm.LogoURL,
		}
		for _, s := range lm.Seasons {
			dirID := s.DirectoryID
			if dirID == "" {
				dirID = s.ID
			}
			if dirID == "" {
				log.Printf("[GetDetails] season %d %q has empty dirID, skipping", s.Number, s.Label)
				continue
			}
			sd.Seasons = append(sd.Seasons, &gen.SeasonInfo{
				Number:       s.Number,
				Label:        s.Label,
				DirectoryId:  dirID,
				PosterUrl:    s.PosterURL,
				Year:         s.Year,
				EpisodeCount: s.EpisodeCount,
				Overview:     s.Overview,
				AirDate:      s.SeasonAirDate,
			})
		}
		if debugLog {
			log.Printf("[GetDetails] SeriesDetails fanartUrl=%q", sd.FanartUrl)
		}
		resp.Details = &gen.DetailsResponse_Series{Series: sd}
	case models.TypeEpisode:
		resp.Details = &gen.DetailsResponse_Episode{Episode: &gen.EpisodeDetails{
			Plot:          lm.Plot,
			ThumbnailUrl:  lm.ThumbnailURL,
			AirDate:       lm.AirDate,
			Duration:      lm.Duration,
			Vote:          lm.Vote,
			EpisodeNumber: lm.EpisodeNumber,
			SeasonNumber:  lm.SeasonNumber,
			ShowId:        lm.ShowID,
		}}
	case models.TypeLive:
		resp.Details = &gen.DetailsResponse_Live{Live: &gen.LiveDetails{
			ChannelName:   lm.ChannelName,
			ChannelLogo:   lm.ChannelLogo,
			StreamStart:   lm.StreamStart,
			StreamEnd:     lm.StreamEnd,
			SportCategory: lm.SportCategory,
			Teams:         lm.Teams,
			Competition:   lm.Competition,
			IsReplay:      lm.IsReplay,
		}}
	case models.TypeVideo:
		resp.Details = &gen.DetailsResponse_Video{Video: &gen.VideoDetails{
			ChannelName: lm.ChannelName,
			ChannelUrl:  lm.ChannelURL,
			PublishedAt: lm.PublishedAt,
			ViewCount:   lm.ViewCount,
			LikeCount:   lm.LikeCount,
			Duration:    lm.VideoDuration,
			Tags:        lm.Tags,
		}}
	case models.TypeMusic:
		resp.Details = &gen.DetailsResponse_Music{Music: &gen.MusicDetails{
			Artist:      lm.Artist,
			Album:       lm.Album,
			AlbumArtUrl: lm.AlbumArtURL,
			TrackNumber: lm.TrackNumber,
			DiscNumber:  lm.DiscNumber,
			Released:    lm.Released,
			Composers:   lm.Composers,
			Genres:      lm.Genres,
			LyricsUrl:   lm.LyricsURL,
		}}
	case models.TypePodcast:
		resp.Details = &gen.DetailsResponse_Podcast{Podcast: &gen.PodcastDetails{
			ShowName:      lm.ShowName,
			Author:        lm.Author,
			PublishedAt:   lm.PublishedAt,
			EpisodeNumber: lm.EpisodeNumber,
			SeasonNumber:  lm.SeasonNumber,
			AudioUrl:      lm.AudioURL,
			Tags:          lm.Tags,
		}}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// luaMeta ↔ gen conversion helpers
// ─────────────────────────────────────────────────────────────────────────────

func luaMetaToCatalogItem(lm luaMeta) *gen.CatalogItem {
	extra := map[string]string{}
	for k, v := range lm.Metadata {
		extra[k] = v
	}
	for k, v := range lm.Extra {
		extra[k] = v
	}
	if lm.Category != "" {
		extra["category"] = lm.Category
	}
	if lm.FanartURL != "" {
		extra["fanart_url"] = lm.FanartURL
	}
	if lm.Plot != "" {
		extra["plot"] = lm.Plot
	}
	if len(lm.Genres) > 0 {
		extra["genres"] = strings.Join(lm.Genres, ",")
	}
	posterURL := lm.PosterURL
	if posterURL == "" && lm.ThumbnailURL != "" {
		posterURL = lm.ThumbnailURL
	}
	return &gen.CatalogItem{
		Id:              lm.ID,
		Title:           lm.Title,
		MediaType:       lm.MediaType,
		PosterUrl:       posterURL,
		Year:            lm.Year,
		Rating:          lm.Rating,
		IsDir:           lm.IsDir,
		ProviderId:      lm.ProviderID,
		ShowId:          lm.ShowID,
		ParentId:        lm.ParentID,
		SeasonNumber:    lm.SeasonNumber,
		EpisodeNumber:   lm.EpisodeNumber,
		DirectStreamUrl: lm.DirectStream,
		IsExternal:      lm.IsExternal,
		Extra:           extra,
		LogoUrl:         lm.LogoURL,
		BannerUrl:       lm.BannerURL,
	}
}

func luaMetasToCatalogItems(lms []luaMeta) []*gen.CatalogItem {
	out := make([]*gen.CatalogItem, 0, len(lms))
	for i, lm := range lms {
		if debugLog && i < 3 {
			log.Printf("[catalog-dbg] item[%d] id=%q poster=%q fanart=%q", i, lm.ID, lm.PosterURL, lm.FanartURL)
		}
		out = append(out, luaMetaToCatalogItem(lm))
	}
	return out
}

func luaMetaToEpisodeInfo(lm luaMeta) *gen.EpisodeInfo {
	return &gen.EpisodeInfo{
		Id:            lm.ID,
		Title:         lm.Title,
		EpisodeNumber: lm.EpisodeNumber,
		SeasonNumber:  lm.SeasonNumber,
		ThumbnailUrl:  lm.ThumbnailURL,
		AirDate:       lm.AirDate,
		Duration:      lm.Duration,
		Vote:          lm.Vote,
		Plot:          lm.Plot,
		ShowId:        lm.ShowID,
		ParentId:      lm.ParentID,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Dispatch helpers
// ─────────────────────────────────────────────────────────────────────────────

// luaCallLuaMetas calls a Lua entrypoint and returns parsed luaMeta items.
// Uses CallEntrypointJSON for a single JSON serialization pass.
func luaCallLuaMetas(pluginID, ep string, args map[string]any, profileID string) ([]luaMeta, error) {
	b, err := engine.LuaPlugins.CallEntrypointJSON(pluginID, ep, args, profileID)
	if err != nil {
		return nil, err
	}
	lms, _, err := luaMetasFromJSON(b)
	return lms, err
}

// luaMetasFromJSON parses a JSON payload as []luaMeta, supporting both a
// plain array and a {items:[], has_more:bool} wrapper from newer plugins.
func luaMetasFromJSON(b json.RawMessage) ([]luaMeta, bool, error) {
	if isNullJSON(b) || string(b) == "{}" {
		return nil, false, nil
	}
	var wrapper struct {
		Items   []luaMeta `json:"items"`
		HasMore bool      `json:"has_more"`
	}
	if err := json.Unmarshal(b, &wrapper); err == nil && wrapper.Items != nil {
		return wrapper.Items, wrapper.HasMore, nil
	}
	var lms []luaMeta
	if err := json.Unmarshal(b, &lms); err == nil {
		return lms, false, nil
	}
	n := min(len(b), 200)
	return nil, false, fmt.Errorf("unexpected lua result shape: %s", string(b[:n]))
}

// isNullJSON reports whether b represents a JSON null or is empty.
func isNullJSON(b json.RawMessage) bool {
	return len(b) == 0 || string(b) == "null"
}
