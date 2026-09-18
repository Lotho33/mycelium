// Package sdk is the public API for Mycelium plugins.
//
// To write a provider plugin in Go:
//
//	type MyProvider struct{}
//	func (p *MyProvider) Init(settings map[string]string) error { ... }
//	// ... implement remaining methods ...
//	func main() { sdk.ServeProvider("com.example.myplugin", &MyProvider{}) }
//
// To write an enricher plugin in Go:
//
//	type MyEnricher struct{}
//	func main() { sdk.ServeEnricher("com.example.myenricher", &MyEnricher{}) }
package sdk

import (
	"context"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Types aligned with the Stremio Addon Protocol
// ─────────────────────────────────────────────────────────────────────────────

// Meta is the primary item type returned by provider methods.
// Stremio-native fields (ID, Type, Name, Poster…) match the protocol directly.
// FunGoBox extension fields carry navigation and enrichment state.

type Link struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	URL      string `json:"url"`
}

type Meta struct {
	// Stremio protocol fields
	ID          string   `json:"id"`
	Type        string   `json:"type"` // "movie", "series", "tv"
	Name        string   `json:"name"`
	Poster      string   `json:"poster,omitempty"`
	Background  string   `json:"background,omitempty"`
	Description string   `json:"description,omitempty"`
	Year        int32    `json:"year,omitempty"`
	Genres      []string `json:"genres,omitempty"`
	IMDBRating  string   `json:"imdbRating,omitempty"`

	Director []string `json:"director,omitempty"`
	Cast     []string `json:"cast,omitempty"`
	Runtime  string   `json:"runtime,omitempty"`
	Links    []Link   `json:"links,omitempty"`

	// Episode fields (leaf items inside series)
	SeasonNumber  int32  `json:"seasonNumber,omitempty"`
	EpisodeNumber int32  `json:"episodeNumber,omitempty"`
	Released      string `json:"released,omitempty"` // ISO 8601 air date for calendar support

	// FunGoBox navigation extensions
	IsDir      bool   `json:"isDir,omitempty"`
	ParentID   string `json:"parentId,omitempty"`
	ShowID     string `json:"showId,omitempty"`
	IsExternal bool   `json:"isExternal,omitempty"`

	// Enricher injection: set by enrichers so the client can stream directly
	DirectStreamURL string `json:"directStreamUrl,omitempty"`

	// Logo / banner — optional rich media assets
	LogoURL   string `json:"logo_url,omitempty"`   // transparent wordmark PNG/SVG
	BannerURL string `json:"banner_url,omitempty"` // wide landscape image

	// Internal tracking (set by core, plugins can leave empty)
	ProviderID string            `json:"providerId,omitempty"`
	Category   string            `json:"category,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// Stream is returned by Resolve(). It mirrors the Stremio Stream object
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

// Catalog is returned by GetSections(). It mirrors the Stremio Catalog object
// extended with preview items for homepage rendering.
type Catalog struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Icon    string `json:"icon,omitempty"`
	Preview []Meta `json:"preview,omitempty"`
}

// SetupInfo is returned by Provider.Setup and carries both the ready flag and
// any runtime capability overrides the plugin wants to declare.
type SetupInfo struct {
	Ready        bool
	Capabilities []string // runtime override of manifest capabilities (may be nil)
	Error        string
}

// ─────────────────────────────────────────────────────────────────────────────
// Plugin interfaces
// ─────────────────────────────────────────────────────────────────────────────

// Provider is the interface every provider plugin must implement.
type Provider interface {
	// Init is called once at startup with the plugin's settings slice.
	Init(settings map[string]string) error

	// Setup validates that the plugin has enough configuration to operate.
	// Return SetupInfo{Ready: false} when settings are incomplete but not erroneous.
	// Capabilities may override manifest declarations at runtime.
	Setup() (SetupInfo, error)

	// Tick is called by Core according to the cron_jobs declared in manifest.json.
	// jobID matches the "id" field of the corresponding cron_job entry.
	// "sync_catalog" is the conventional ID for catalog refresh jobs.
	Tick(ctx context.Context, jobID string) error

	// GetSections returns all top-level catalogs for this provider.
	// Each catalog may include preview items so the client can render the homepage
	// without additional Browse calls.
	GetSections(ctx context.Context) ([]*Catalog, error)

	// Search returns items matching query with optional year/genre filters (paged, 1-based).
	// year=0 and genre="" mean no filter.
	Search(ctx context.Context, query string, year int, genre string, page int) ([]*Meta, error)

	// Browse returns children of directoryID (empty string = root), paged (1-based).
	Browse(ctx context.Context, directoryID string, page int) ([]*Meta, error)

	// Resolve resolves a playable ID to a stream.
	Resolve(ctx context.Context, playableID string) (*Stream, error)

	// GetMeta returns full metadata for a single item by its ID.
	// Returns nil, ErrNotFound if the item is not known to this provider.
	GetMeta(ctx context.Context, itemID string) (*Meta, error)
}

// Enricher is the interface every enricher plugin must implement.
type Enricher interface {
	// Init is called once at startup with settings.
	Init(settings map[string]string) error

	// Setup validates the enricher's configuration.
	Setup() (SetupInfo, error)

	// Enrich receives a slice of items from providers and may reorder,
	// annotate, or filter them. Must return a non-nil slice.
	Enrich(ctx context.Context, items []*Meta) ([]*Meta, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal proto conversion helpers (used by serve.go)
// ─────────────────────────────────────────────────────────────────────────────

// metaRatingFloat parses the IMDBRating string to a float64 for the proto wire.
func metaRatingFloat(r string) float64 {
	if r == "" {
		return 0
	}
	var f float64
	fmt.Sscanf(r, "%f", &f)
	return f
}

// protoTypeToStremio normalises old internal mediatypes to Stremio types.
// Plugins using this SDK already use Stremio types, but old plugins on the wire
// may still send "tvshow", "episode", etc.
func protoTypeToStremio(mt string) string {
	switch strings.ToLower(mt) {
	case "movie":
		return "movie"
	case "tvshow", "series", "season", "episode":
		return "series"
	case "channel", "tv", "live":
		return "tv"
	default:
		return mt
	}
}
