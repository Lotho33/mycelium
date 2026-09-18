// Package models defines plain Go structs for the Mycelium domain types.
package models

// Media type constants — use these instead of bare strings.
const (
	TypeMovie   = "movie"
	TypeSeries  = "series"
	TypeEpisode = "episode"
	TypeLive    = "live" // live TV / sport
	TypeMusic   = "music"
	TypePodcast = "podcast"
	TypeVideo   = "video" // generic video (YouTube-like)
)

// Meta is the lightweight tile object used in catalog/search/browse.
// Rich per-type metadata lives in the typed Details structs below.
type Meta struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"` // see TypeX constants above
	Name        string   `json:"name"`
	Poster      string   `json:"poster,omitempty"`
	Background  string   `json:"background,omitempty"`
	Description string   `json:"description,omitempty"`
	Year        int32    `json:"year,omitempty"`
	Genres      []string `json:"genres,omitempty"`
	IMDBRating  string   `json:"imdbRating,omitempty"`

	AudioTracks    []string `json:"audioTracks,omitempty"`
	SubtitleTracks []string `json:"subtitleTracks,omitempty"`

	// Episode navigation
	SeasonNumber  int32 `json:"seasonNumber,omitempty"`
	EpisodeNumber int32 `json:"episodeNumber,omitempty"`

	// Navigation
	IsDir      bool   `json:"isDir,omitempty"`
	ParentID   string `json:"parentId,omitempty"`
	ShowID     string `json:"showId,omitempty"`
	IsExternal bool   `json:"isExternal,omitempty"`

	DirectStreamURL string `json:"directStreamUrl,omitempty"`

	LogoURL   string `json:"logo_url,omitempty"`   // transparent wordmark PNG/SVG
	BannerURL string `json:"banner_url,omitempty"` // wide landscape image

	ProviderID string            `json:"providerId,omitempty"`
	Category   string            `json:"category,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// MovieDetails holds rich metadata for TypeMovie items.
type MovieDetails struct {
	Runtime       string   `json:"runtime,omitempty"`
	Cast          []string `json:"cast,omitempty"`
	Directors     []string `json:"directors,omitempty"`
	TrailerURL    string   `json:"trailer_url,omitempty"`
	Released      string   `json:"released,omitempty"`
	Tagline       string   `json:"tagline,omitempty"`
	ContentRating string   `json:"content_rating,omitempty"`
}

// SeriesDetails holds rich metadata for TypeSeries items.
type SeriesDetails struct {
	Cast          []string `json:"cast,omitempty"`
	Directors     []string `json:"directors,omitempty"`
	TrailerURL    string   `json:"trailer_url,omitempty"`
	Status        string   `json:"status,omitempty"`
	TotalSeasons  int32    `json:"total_seasons,omitempty"`
	TotalEpisodes int32    `json:"total_episodes,omitempty"`
	ContentRating string   `json:"content_rating,omitempty"`
	Network       string   `json:"network,omitempty"`
}

// LiveDetails holds metadata for TypeLive items.
type LiveDetails struct {
	ChannelName   string   `json:"channel_name,omitempty"`
	ChannelLogo   string   `json:"channel_logo,omitempty"`
	StreamStart   string   `json:"stream_start,omitempty"`
	StreamEnd     string   `json:"stream_end,omitempty"`
	SportCategory string   `json:"sport_category,omitempty"`
	Teams         []string `json:"teams,omitempty"`
	Competition   string   `json:"competition,omitempty"`
	IsReplay      bool     `json:"is_replay,omitempty"`
}

// VideoDetails holds metadata for TypeVideo (YouTube-like) items.
type VideoDetails struct {
	ChannelName string   `json:"channel_name,omitempty"`
	ChannelURL  string   `json:"channel_url,omitempty"`
	PublishedAt string   `json:"published_at,omitempty"`
	ViewCount   int64    `json:"view_count,omitempty"`
	LikeCount   int64    `json:"like_count,omitempty"`
	Duration    string   `json:"duration,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// MusicDetails holds metadata for TypeMusic items.
type MusicDetails struct {
	Artist      string   `json:"artist,omitempty"`
	Album       string   `json:"album,omitempty"`
	AlbumArtURL string   `json:"album_art_url,omitempty"`
	Duration    string   `json:"duration,omitempty"`
	TrackNumber int32    `json:"track_number,omitempty"`
	DiscNumber  int32    `json:"disc_number,omitempty"`
	Released    string   `json:"released,omitempty"`
	Composers   []string `json:"composers,omitempty"`
	Genres      []string `json:"genres,omitempty"`
	LyricsURL   string   `json:"lyrics_url,omitempty"`
}

// PodcastDetails holds metadata for TypePodcast items.
type PodcastDetails struct {
	ShowName      string   `json:"show_name,omitempty"`
	Author        string   `json:"author,omitempty"`
	PublishedAt   string   `json:"published_at,omitempty"`
	Duration      string   `json:"duration,omitempty"`
	EpisodeNumber int32    `json:"episode_number,omitempty"`
	SeasonNumber  int32    `json:"season_number,omitempty"`
	AudioURL      string   `json:"audio_url,omitempty"`
	Tags          []string `json:"tags,omitempty"`
}

// Stream is what Resolve() returns. It mirrors the Stremio Stream object
// with additional fields for proxy headers and playback hints.
type Stream struct {
	URL         string            `json:"url"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	IsLive      bool              `json:"isLive,omitempty"`
	IsResumable bool              `json:"isResumable,omitempty"`
	DirectPlay  bool              `json:"directPlay,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Catalog is what GetSections() returns. It mirrors the Stremio Catalog object
// extended with preview items for homepage rendering.
type Catalog struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Icon    string `json:"icon,omitempty"`
	Preview []Meta `json:"preview,omitempty"`
}
