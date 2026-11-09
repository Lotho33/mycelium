package pileus

import (
	"context"
	"log"
	"sync"
	"time"

	"mycelium/internal/core"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/protobuf/proto"
)

// Catalog pages are cached twice: a fresh copy that lives for the catalog's
// TTL, and a stale copy that outlives it (staleCatalogTTL). When the fresh
// copy has expired the stale one is served at once and the page is refreshed
// in the background, one refresh per page at a time.
//
// Without it, an expired page is fetched inline: if the plugin's catalog
// budget is held by a long cron task (or the upstream is slow) the carousel
// waits out the whole timeout and then shows nothing, although a copy a few
// minutes old was good enough.

// catalogStore is the slice of the Redis repo the catalog cache uses.
type catalogStore interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
}

// catalogFetch loads a catalog page from the plugin.
type catalogFetch func(ctx context.Context) ([]*gen.CatalogItem, bool, error)

const (
	// staleCatalogMin / staleCatalogMax bound how long a stale copy is kept.
	staleCatalogMin = 10 * time.Minute
	staleCatalogMax = 6 * time.Hour
	// staleCatalogFactor: a stale copy lives this many TTLs. A live catalog
	// with a 90 s TTL keeps a stale copy for 12 min; a 15 min one
	// for 2 h.
	staleCatalogFactor = 8
	// catalogRefreshTimeout bounds a background refresh (the entrypoint's own
	// timeout applies inside it too).
	catalogRefreshTimeout = 2 * time.Minute
	catalogStoreTimeout   = 2 * time.Second
)

func staleCatalogTTL(ttl time.Duration) time.Duration {
	return min(max(ttl*staleCatalogFactor, staleCatalogMin), staleCatalogMax)
}

func catalogStaleKey(freshKey string) string { return freshKey + ":stale" }

// catalogRefreshing holds the fresh keys whose background refresh is running.
var catalogRefreshing sync.Map

// readCatalogPage reads a cached page; ok=false on a miss or a corrupt entry.
func readCatalogPage(ctx context.Context, store catalogStore, key string) (*gen.CatalogResponse, bool) {
	raw, err := store.Get(ctx, key)
	if err != nil || raw == "" {
		return nil, false
	}
	resp := &gen.CatalogResponse{}
	if proto.Unmarshal([]byte(raw), resp) != nil {
		log.Printf("[catalog-cache] voce corrotta ignorata: %s", key)
		return nil, false
	}
	return resp, true
}

// storeCatalogPage writes both copies of a page, fire-and-forget. Empty
// pages aren't cached: they are as often a transient failure.
func storeCatalogPage(store catalogStore, key string, resp *gen.CatalogResponse, ttl time.Duration) {
	if store == nil || len(resp.Items) == 0 {
		return
	}
	blob, err := proto.Marshal(resp)
	if err != nil {
		return
	}
	core.SafeGo("pileus/catalog-cache-store", func() {
		ctx, cancel := context.WithTimeout(context.Background(), catalogStoreTimeout)
		defer cancel()
		_ = store.Set(ctx, key, string(blob), ttl)
		_ = store.Set(ctx, catalogStaleKey(key), string(blob), staleCatalogTTL(ttl))
	})
}

// cachedCatalogPage serves a catalog page: fresh copy, else stale copy plus a
// background refresh, else an inline fetch. store may be nil (no Redis).
func cachedCatalogPage(ctx context.Context, store catalogStore, key string, ttl time.Duration, fetch catalogFetch) (resp *gen.CatalogResponse, err error) {
	if store != nil {
		if resp, ok := readCatalogPage(ctx, store, key); ok {
			return resp, nil
		}
		if resp, ok := readCatalogPage(ctx, store, catalogStaleKey(key)); ok {
			refreshCatalogPage(store, key, ttl, fetch)
			return resp, nil
		}
	}
	items, hasMore, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	resp = &gen.CatalogResponse{Items: items, HasMore: hasMore}
	storeCatalogPage(store, key, resp, ttl)
	return resp, nil
}

// refreshCatalogPage refetches a page in the background, at most one
// refresh per page at a time. A failed refresh keeps the stale copy.
func refreshCatalogPage(store catalogStore, key string, ttl time.Duration, fetch catalogFetch) {
	if _, running := catalogRefreshing.LoadOrStore(key, struct{}{}); running {
		return
	}
	core.SafeGo("pileus/catalog-refresh", func() {
		defer catalogRefreshing.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), catalogRefreshTimeout)
		defer cancel()
		items, hasMore, err := fetch(ctx)
		if err != nil {
			log.Printf("[catalog-cache] refresh in background fallito (resta la copia precedente) %s: %v", key, err)
			return
		}
		storeCatalogPage(store, key, &gen.CatalogResponse{Items: items, HasMore: hasMore}, ttl)
	})
}
