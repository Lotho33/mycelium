package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/engine"
)

func TestNormalizeRepoSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Lotho33/pileus":                                 "Lotho33/pileus",
		"Lotho33/pileus.git":                             "Lotho33/pileus",
		"https://github.com/Lotho33/pileus":              "Lotho33/pileus",
		"github.com/Lotho33/pileus/":                     "Lotho33/pileus",
		"https://github.com/Lotho33/pileus/releases/tag": "Lotho33/pileus",
		"https://www.github.com/a/b.git":                 "a/b",
	} {
		if got, ok := normalizeRepoSlug(in); !ok || got != want {
			t.Errorf("normalizeRepoSlug(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "pileus", "https://gitlab.com/a/b", "https://github.com/a", "a/b/c d"} {
		if got, ok := normalizeRepoSlug(bad); ok {
			t.Errorf("normalizeRepoSlug(%q) = %q, want rejected", bad, got)
		}
	}
}

func TestParsePluginSource(t *testing.T) {
	if p, err := parsePluginSource("https://example.org/plugins/x.zip"); err != nil || p.URL == "" {
		t.Fatalf("direct zip: %+v %v", p, err)
	}
	// A release asset link is a file, not the repository.
	if p, err := parsePluginSource("https://github.com/o/r/releases/download/v1/x.zip"); err != nil || p.URL == "" {
		t.Fatalf("asset link: %+v %v", p, err)
	}
	if p, err := parsePluginSource("https://github.com/o/r"); err != nil || p.Repo != "o/r" {
		t.Fatalf("repo url: %+v %v", p, err)
	}
	if _, err := parsePluginSource("ftp://x/y.zip"); err == nil {
		t.Fatal("ftp accepted")
	}
}

func TestPickCandidate(t *testing.T) {
	cands := []candidate{{Name: "alpha.zip"}, {Name: "beta.zip"}}
	if c, err := pickCandidate(cands, "beta", ""); err != nil || c.Name != "beta.zip" {
		t.Fatalf("by id: %+v %v", c, err)
	}
	if c, err := pickCandidate(cands, "other", "alpha.zip"); err != nil || c.Name != "alpha.zip" {
		t.Fatalf("by asset: %+v %v", c, err)
	}
	if _, err := pickCandidate(cands, "other", ""); err == nil {
		t.Fatal("ambiguous release accepted")
	}
}

// fakeRelease serves a GitHub API with one release holding the given ZIP
// assets, whose tag and content the test can change.
type fakeRelease struct {
	srv    *httptest.Server
	tag    atomic.Value // string
	assets atomic.Value // map[string][]byte
}

func newFakeRelease(t *testing.T) *fakeRelease {
	f := &fakeRelease{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assets := f.assets.Load().(map[string][]byte)
		switch {
		case r.URL.Path == "/repos/o/r/releases/latest":
			var list []map[string]any
			for name := range assets {
				list = append(list, map[string]any{"name": name, "browser_download_url": f.srv.URL + "/dl/" + name})
			}
			json.NewEncoder(w).Encode(map[string]any{"tag_name": f.tag.Load().(string), "assets": list})
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			w.Write(assets[strings.TrimPrefix(r.URL.Path, "/dl/")])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	t.Cleanup(core.SetGitHubAPIBaseForTest(f.srv.URL))
	orig := checkSourceURL
	checkSourceURL = func(context.Context, string) error { return nil }
	t.Cleanup(func() { checkSourceURL = orig })
	return f
}

func pluginZip(t *testing.T, id, version, extra string) []byte {
	return buildZipBytes(t, map[string]string{
		id + "/manifest.yaml": "id: " + id + "\nname: T\nversion: \"" + version + "\"\ndirect_egress: true\nentrypoints:\n  probe: probe\n",
		id + "/init.lua":      "function probe() return " + extra + ", nil end\n",
	})
}

// Install from a repository, then a newer release updates it; a release
// whose archive holds another plugin is refused.
func TestPluginSource_InstallAndUpdateFromRelease(t *testing.T) {
	tmp := t.TempDir()
	oldBase := core.BasePath
	core.BasePath = tmp
	t.Cleanup(func() { core.BasePath = oldBase })
	if err := os.MkdirAll(core.AppPath("plugins"), 0o755); err != nil {
		t.Fatal(err)
	}

	const id = "srcplug"
	f := newFakeRelease(t)
	f.tag.Store("v1")
	f.assets.Store(map[string][]byte{id + ".zip": pluginZip(t, id, "1.0.0", "1")})
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin(id); forgetPluginSource(id) })

	done, errs := installFromSource(context.Background(), parsedSource{Repo: "o/r"}, true)
	if len(errs) > 0 || len(done) != 1 || done[0].ID != id {
		t.Fatalf("install: %+v %v", done, errs)
	}
	src := loadPluginSources()[id]
	if src.Source != "o/r" || src.Version != "v1" || !src.AutoUpdate || src.Asset != id+".zip" {
		t.Fatalf("source not remembered: %+v", src)
	}

	// Same release: nothing to do.
	if updated, err := checkPluginSource(context.Background(), id); err != nil || updated {
		t.Fatalf("same release: updated=%v err=%v", updated, err)
	}

	// A new release: installed.
	f.tag.Store("v2")
	f.assets.Store(map[string][]byte{id + ".zip": pluginZip(t, id, "2.0.0", "2")})
	if updated, err := checkPluginSource(context.Background(), id); err != nil || !updated {
		t.Fatalf("new release: updated=%v err=%v", updated, err)
	}
	if v := loadPluginSources()[id].Version; v != "v2" {
		t.Fatalf("version after update = %q", v)
	}
	var installed string
	for _, mf := range engine.LuaPlugins.GetMeta() {
		if mf.ID == id {
			installed = mf.Version
		}
	}
	if installed != "2.0.0" {
		t.Fatalf("installed version = %q, want 2.0.0", installed)
	}

	// A release that now ships another plugin under the same asset name.
	f.tag.Store("v3")
	f.assets.Store(map[string][]byte{id + ".zip": pluginZip(t, "intruder", "9.9.9", "3")})
	if _, err := checkPluginSource(context.Background(), id); err == nil || !strings.Contains(err.Error(), "intruder") {
		t.Fatalf("other plugin accepted: %v", err)
	}
	if engine.LuaPlugins.Has("intruder") {
		t.Fatal("intruder installed")
	}
	if loadPluginSources()[id].LastError == "" {
		t.Fatal("failed check not recorded")
	}
}
