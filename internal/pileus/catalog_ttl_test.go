package pileus

import (
	"testing"
	"time"

	"mycelium/internal/engine"
)

func TestDynamicCatalogTTL(t *testing.T) {
	rememberCatalogTTLs("p", []engine.LuaCatalogDef{
		{ID: "sport_cat::football", CacheTTLSeconds: 90},
		{ID: "no_ttl"},
	})
	if ttl, ok := dynamicCatalogTTL("p", "sport_cat::football"); !ok || ttl != 90*time.Second {
		t.Fatalf("ttl = %v %v", ttl, ok)
	}
	if _, ok := dynamicCatalogTTL("p", "no_ttl"); ok {
		t.Fatal("a catalog without cache_ttl_seconds must keep the default")
	}
	if _, ok := dynamicCatalogTTL("other", "sport_cat::football"); ok {
		t.Fatal("TTLs are per plugin")
	}
}
