package pileus

import (
	"sync"
	"time"

	"mycelium/internal/engine"
)

// Cache lifetimes of catalogs a plugin builds at run time (catalog_list),
// which the manifest doesn't list. ListPlugins resolves those lists anyway;
// it records each catalog's cache_ttl_seconds here, so the response cache of
// a live category row honours its short TTL instead of the 15-minute default.
var dynamicCatalogTTLs = struct {
	sync.RWMutex
	m map[string]time.Duration // pluginID + "\x00" + catalogID
}{m: map[string]time.Duration{}}

// dynamicCatalogDefault applies to a catalog_list catalog not seen yet (e.g.
// right after a restart, before the first ListPlugins).
const dynamicCatalogDefault = 5 * time.Minute

func rememberCatalogTTLs(pluginID string, defs []engine.LuaCatalogDef) {
	dynamicCatalogTTLs.Lock()
	defer dynamicCatalogTTLs.Unlock()
	for _, d := range defs {
		if d.CacheTTLSeconds > 0 {
			dynamicCatalogTTLs.m[pluginID+"\x00"+d.ID] = time.Duration(d.CacheTTLSeconds) * time.Second
		}
	}
}

func dynamicCatalogTTL(pluginID, catalogID string) (time.Duration, bool) {
	dynamicCatalogTTLs.RLock()
	defer dynamicCatalogTTLs.RUnlock()
	ttl, ok := dynamicCatalogTTLs.m[pluginID+"\x00"+catalogID]
	return ttl, ok
}
