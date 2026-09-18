package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/engine"
)

func callWipePluginDataFor(t *testing.T, pluginID, password string) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Buffer
	if password == "" {
		body = bytes.NewBufferString(`{}`)
	} else {
		b, err := json.Marshal(map[string]string{"password": password})
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		body = bytes.NewBuffer(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/lua-plugins/wipe-data/"+pluginID, body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("plugin_id", pluginID)
	rec := httptest.NewRecorder()
	wipePluginDataFor(rec, req)
	return rec
}

func TestWipePluginDataFor_UnknownPluginReturns404(t *testing.T) {
	rec := callWipePluginDataFor(t, "does.not.exist", "whatever")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", rec.Code)
	}
}

func TestWipePluginDataFor_PathTraversalRejected(t *testing.T) {
	rec := callWipePluginDataFor(t, "../etc", "whatever")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 (traversal-looking id must never reach engine.LuaPlugins.Has or the filesystem)", rec.Code)
	}
}

// loadPluginAt loads a minimal Lua plugin whose files really live under
// core.AppPath("plugins", id) — unlike the loadIconPlugin helper
// (plugin_icon_test.go), which loads from an unrelated t.TempDir() and is
// fine for testing routes that only care about engine registration, but
// wrong here: wipePluginDataFor removes files at core.AppPath("plugins",
// id), so the test plugin's real files need to live there too.
func loadPluginAt(t *testing.T, id string) string {
	t.Helper()
	dir := core.AppPath("plugins", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "id: " + id + "\nname: T\ndirect_egress: true\nentrypoints:\n  probe: probe\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init.lua"), []byte("function probe() return {} end"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := engine.LuaPlugins.LoadPlugin(dir); err != nil {
		t.Fatalf("LoadPlugin(%s): %v", dir, err)
	}
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin(id) })
	return dir
}

func TestWipePluginDataFor_WrongPasswordRejectedNothingDeleted(t *testing.T) {
	tmp := t.TempDir()
	oldBase := core.BasePath
	core.BasePath = tmp
	t.Cleanup(func() { core.BasePath = oldBase })
	if err := os.MkdirAll(core.AppPath("plugins"), 0o755); err != nil {
		t.Fatal(err)
	}

	seedAdminPassword(t, "correct-horse-battery")
	dir := loadPluginAt(t, "wipetest.wrongpw")

	if err := os.WriteFile(filepath.Join(dir, "catalog_cache.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := callWipePluginDataFor(t, "wipetest.wrongpw", "wrong-password")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body=%s; want 401", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "catalog_cache.json")); err != nil {
		t.Errorf("catalog_cache.json was removed despite the wrong password: %v", err)
	}
}

// TestWipePluginDataFor_OnlyTargetPluginsCacheFilesRemoved is the core
// regression test: two plugins each have their own cache files on disk: the
// endpoint must remove only the requested plugin's, leaving the other
// completely untouched — the whole point of this endpoint over the existing
// global wipePluginDataHandler.
func TestWipePluginDataFor_OnlyTargetPluginsCacheFilesRemoved(t *testing.T) {
	tmp := t.TempDir()
	oldBase := core.BasePath
	core.BasePath = tmp
	t.Cleanup(func() { core.BasePath = oldBase })
	if err := os.MkdirAll(core.AppPath("plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	seedAdminPassword(t, "correct-horse-battery")

	dirA := loadPluginAt(t, "wipetest.a")
	dirB := loadPluginAt(t, "wipetest.b")
	for _, f := range []string{"catalog_cache.json", "logo_cache.json", "fribb_index.json", "avail_cache.json"} {
		if err := os.WriteFile(filepath.Join(dirA, f), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirB, f), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A file the wipe must never touch regardless of plugin.
	if err := os.WriteFile(filepath.Join(dirA, "init.lua"), []byte("function probe() return {} end"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := callWipePluginDataFor(t, "wipetest.a", "correct-horse-battery")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK         bool   `json:"ok"`
		PluginID   string `json:"plugin_id"`
		CacheFiles int    `json:"cache_files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.PluginID != "wipetest.a" || resp.CacheFiles != 4 {
		t.Fatalf("response = %+v; want ok=true plugin_id=wipetest.a cache_files=4", resp)
	}

	// Plugin A's 4 cache files gone, init.lua untouched.
	for _, f := range []string{"catalog_cache.json", "logo_cache.json", "fribb_index.json", "avail_cache.json"} {
		if _, err := os.Stat(filepath.Join(dirA, f)); !os.IsNotExist(err) {
			t.Errorf("wipetest.a's %s still exists after wipe", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dirA, "init.lua")); err != nil {
		t.Errorf("wipetest.a's init.lua was removed by a data wipe: %v", err)
	}

	// Plugin B (not targeted) must be completely untouched.
	for _, f := range []string{"catalog_cache.json", "logo_cache.json", "fribb_index.json", "avail_cache.json"} {
		if _, err := os.Stat(filepath.Join(dirB, f)); err != nil {
			t.Errorf("wipetest.b's %s was removed by a wipe targeted at wipetest.a: %v", f, err)
		}
	}

	if !engine.LuaPlugins.Has("wipetest.a") || !engine.LuaPlugins.Has("wipetest.b") {
		t.Errorf("both plugins should remain installed/loaded after a data-only wipe")
	}
}
