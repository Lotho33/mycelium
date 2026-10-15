package pileus

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mycelium/internal/engine"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
)

func loadFiltersPlugin(t *testing.T, id, manifestExtra, src string) *luaPipelineProvider {
	t.Helper()
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "id: " + id + "\nname: T\nversion: \"1\"\ndirect_egress: true\npool_size: 1\nentrypoints:\n  catalog: get_catalog\n" + manifestExtra
	os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(dir, "init.lua"), []byte(src), 0o644)
	if err := engine.LuaPlugins.LoadPlugin(dir); err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin(id) })
	return &luaPipelineProvider{id: id}
}

const filtersSrc = `
calls = 0
function get_catalog(args) return { items = {}, has_more = false } end
function get_search_filters()
  calls = calls + 1
  return { { id = "genre", label = "Generi", type = "multiselect", options = { { id = "1", label = "A" } } } }
end
function filter_calls() return { n = calls } end
`

// No filters declared: an empty list, without calling the plugin.
func TestSearchFilters_NotDeclared(t *testing.T) {
	p := loadFiltersPlugin(t, "t.nofilters", "", filtersSrc)
	f, err := p.GetSearchFilters(context.Background())
	if err != nil || len(f) != 0 {
		t.Fatalf("got %v, %v; want no filters and no error", f, err)
	}
}

// Served from the cache after the first call; warmed by the first catalog.
func TestSearchFilters_CachedAndWarmed(t *testing.T) {
	p := loadFiltersPlugin(t, "t.filters", "  search_filters: get_search_filters\n  filter_calls: filter_calls\n", filtersSrc)
	calls := func() string {
		raw, err := engine.LuaPlugins.CallEntrypointJSON(p.id, "filter_calls", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	// The catalog warms the cache in the background.
	if _, _, err := p.GetCatalog(context.Background(), "c", 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := searchFiltersCached(p.id, "1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("catalog request never warmed the filters")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		f, err := p.GetSearchFilters(context.Background())
		if err != nil || len(f) != 1 || f[0].Id != "genre" {
			t.Fatalf("got %v, %v", f, err)
		}
	}
	if c := calls(); c != `{"n":1}` {
		t.Fatalf("plugin calls = %s, want 1 (cache)", c)
	}
}

// A plugin that has filters but fails answers UNAVAILABLE, not "no filters".
func TestSearchFilters_FailureIsUnavailable(t *testing.T) {
	p := loadFiltersPlugin(t, "t.failfilters", "  search_filters: get_search_filters\n", `
function get_catalog(args) return { items = {}, has_more = false } end
function get_search_filters() error("upstream down") end
`)
	_, err := p.GetSearchFilters(context.Background())
	if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Fatalf("err = %v, want UNAVAILABLE", err)
	}
}

// Past the TTL the last filters are still served at once (and refreshed in
// the background); a failing refresh never takes them away.
func TestSearchFilters_StaleServedWhileRefreshing(t *testing.T) {
	p := loadFiltersPlugin(t, "t.stalefilters", "  search_filters: get_search_filters\n", `
function get_catalog(args) return { items = {}, has_more = false } end
function get_search_filters() error("upstream down") end
`)
	old := []*gen.SearchFilter{{Id: "genre", Label: "Generi", Type: "multiselect"}}
	searchFiltersMu.Lock()
	searchFiltersCache[p.id] = searchFiltersEntry{version: "1", filters: old, at: time.Now().Add(-2 * searchFiltersTTL)}
	searchFiltersMu.Unlock()
	t.Cleanup(func() {
		searchFiltersMu.Lock()
		delete(searchFiltersCache, p.id)
		searchFiltersMu.Unlock()
	})

	for i := 0; i < 2; i++ {
		f, err := p.GetSearchFilters(context.Background())
		if err != nil || len(f) != 1 || f[0].Id != "genre" {
			t.Fatalf("call %d: got %v, %v; want the stale filters", i, f, err)
		}
		time.Sleep(50 * time.Millisecond) // let the background refresh fail
	}
}

// Startup warm-up fills the cache of every plugin that declares filters.
func TestSearchFilters_WarmAll(t *testing.T) {
	p := loadFiltersPlugin(t, "t.warmall", "  search_filters: get_search_filters\n", filtersSrc)
	t.Cleanup(func() {
		searchFiltersMu.Lock()
		delete(searchFiltersCache, p.id)
		searchFiltersMu.Unlock()
	})
	WarmAllSearchFilters()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := searchFiltersCached(p.id, "1"); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("WarmAllSearchFilters never filled the cache")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
