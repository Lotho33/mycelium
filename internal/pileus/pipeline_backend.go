package pileus

import (
	"context"

	"mycelium/internal/engine"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PipelineProvider is the unified abstraction over both Lua and gRPC plugins.
// It maps 1:1 to the MediaPipeline gRPC surface so that media_handler.go
// contains no plugin-type branching — every handler resolves the backend once
// and delegates to the returned PipelineProvider.
type PipelineProvider interface {
	// GetCatalog returns items for a named catalog (paged, 1-based).
	GetCatalog(ctx context.Context, catalogID string, page int) ([]*gen.CatalogItem, bool, error)

	// GetSearchFilters returns filter definitions; nil, nil for unsupported providers.
	GetSearchFilters(ctx context.Context) ([]*gen.SearchFilter, error)

	// Search returns items matching query and optional filters (paged, 1-based).
	Search(ctx context.Context, query string, page int, filters map[string]string) ([]*gen.CatalogItem, bool, error)

	// GetDetails returns full per-type metadata including typed details (oneof).
	GetDetails(ctx context.Context, mediaID string) (*gen.DetailsResponse, error)

	// Browse returns children of dirID (season directory, episode group, …).
	// Episodes are placed in BrowseResponse.Episodes; directories in Items.
	Browse(ctx context.Context, dirID string, page int) (*gen.BrowseResponse, error)

	// GetStreams returns available stream sources for a playable item.
	GetStreams(ctx context.Context, mediaID string) ([]*gen.StreamSource, error)

	// ResolveStream returns the raw stream URL + metadata.
	// HLS proxy rewriting and skip-time injection are applied by the MediaHandler
	// after this call, not by the backend.
	// extra carries plugin-specific fields (mal_id, introdb_api_key, tmdb_id, …).
	// onProgress, if non-nil, is forwarded status updates the plugin emits while
	// still resolving (e.g. Lua plugins via mycelium.progress()); backends that
	// can't support it simply ignore it.
	ResolveStream(ctx context.Context, streamID string, onProgress engine.ProgressFunc) (resolvedURL string, headers map[string]string, isLive bool, extra map[string]string, err error)
}

// lookupBackend returns the PipelineProvider for pluginID. All plugins are Lua.
func lookupBackend(pluginID string) (PipelineProvider, error) {
	// A plugin toggled off via SetRunEnabled reports !IsOperational, so a client
	// with a stale plugin list (or a direct GetCatalog/Search/Browse/GetStreams
	// call) is refused here rather than served from a "disabled" plugin.
	if !engine.LuaPlugins.IsOperational(pluginID) || !engine.LuaPlugins.Has(pluginID) {
		return nil, status.Errorf(codes.NotFound, "plugin %q not found or not ready", pluginID)
	}
	return &luaPipelineProvider{id: pluginID}, nil
}
