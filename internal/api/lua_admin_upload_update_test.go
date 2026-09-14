package api

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/engine"
)

// buildZipBytes is buildZip (zip_extract_test.go) minus the *zip.Reader
// wrapping — uploadLuaPlugin needs raw bytes to put in a multipart body.
func buildZipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func postLuaPluginZip(t *testing.T, filename string, zipBytes []byte) *httptest.ResponseRecorder {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("plugin_file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(zipBytes); err != nil {
		t.Fatalf("write zip into form: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/lua-plugins/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	uploadLuaPlugin(rec, req)
	return rec
}

// TestUploadLuaPlugin_UpdatesExistingPluginByManifestID is the regression
// test for the fix requested by the user: uploading a ZIP whose *filename*
// does not match an already-installed plugin's directory name must still be
// treated as an update to that plugin, keyed on the id declared inside
// manifest.yaml — not left to silently create a second, separate plugin.
// It also asserts the property the update relies on to be safe to use
// repeatedly: a runtime file the new ZIP doesn't include (standing in for a
// plugin's own on-disk cache, e.g. catalog_cache.json) survives untouched,
// while a file present in both old and new gets genuinely replaced.
func TestUploadLuaPlugin_UpdatesExistingPluginByManifestID(t *testing.T) {
	tmp := t.TempDir()
	oldBase := core.BasePath
	core.BasePath = tmp
	t.Cleanup(func() { core.BasePath = oldBase })

	pluginsDir := core.AppPath("plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}

	// Simulate an existing installation: directory name matches the id here,
	// but that's incidental — the fix must key off manifest.yaml, not this
	// directory name, once an update comes in under a different zip name.
	const id = "testplug"
	installDir := filepath.Join(pluginsDir, id)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("mkdir install dir: %v", err)
	}
	manifest := "id: " + id + "\nname: T\ndirect_egress: true\nentrypoints:\n  probe: probe\n"
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(installDir, rel), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("manifest.yaml", manifest)
	write("init.lua", "function probe() return {v=1} end")
	write("runtime_cache.json", `{"stale":false}`) // stands in for catalog_cache.json etc.

	if err := engine.LuaPlugins.LoadPlugin(installDir); err != nil {
		t.Fatalf("initial LoadPlugin: %v", err)
	}
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin(id) })

	// "Update" ZIP: deliberately named nothing like the plugin id or its
	// install directory — this is exactly the scenario the old
	// filename-derived destDir got wrong. Contains a changed init.lua and
	// NOT runtime_cache.json.
	updateZip := buildZipBytes(t, map[string]string{
		"manifest.yaml": manifest,
		"init.lua":      "function probe() return {v=2} end",
	})

	rec := postLuaPluginZip(t, "totally-unrelated-name-v2.zip", updateZip)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"update":"true"`)) {
		t.Errorf("response = %s; want update:true (this must be reported as an update, not a fresh install)", rec.Body.String())
	}

	// Must NOT have created a second plugin directory named after the zip.
	if _, err := os.Stat(filepath.Join(pluginsDir, "totally-unrelated-name-v2")); !os.IsNotExist(err) {
		t.Errorf("a second directory was created from the zip filename instead of updating %q in place", id)
	}

	// The existing plugin's own directory must now hold the *new* init.lua…
	got, err := os.ReadFile(filepath.Join(installDir, "init.lua"))
	if err != nil {
		t.Fatalf("read updated init.lua: %v", err)
	}
	if string(got) != "function probe() return {v=2} end" {
		t.Errorf("init.lua = %q; want the new content from the update ZIP", got)
	}

	// …while a file the update ZIP didn't include must survive untouched —
	// the property that makes "just re-upload a fresh zip" safe to do
	// without wiping a plugin's already-populated on-disk cache.
	cache, err := os.ReadFile(filepath.Join(installDir, "runtime_cache.json"))
	if err != nil {
		t.Fatalf("runtime_cache.json disappeared across the update: %v", err)
	}
	if string(cache) != `{"stale":false}` {
		t.Errorf("runtime_cache.json = %q; want it untouched by the update", cache)
	}

	if !engine.LuaPlugins.Has(id) {
		t.Errorf("plugin %q not registered after update", id)
	}
}

// TestUploadLuaPlugin_FreshInstallReportsNotAnUpdate is the negative
// counterpart: a genuinely new plugin id must not be misreported as an
// update.
func TestUploadLuaPlugin_FreshInstallReportsNotAnUpdate(t *testing.T) {
	tmp := t.TempDir()
	oldBase := core.BasePath
	core.BasePath = tmp
	t.Cleanup(func() { core.BasePath = oldBase })

	if err := os.MkdirAll(core.AppPath("plugins"), 0o755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}

	const id = "brandnew"
	zipBytes := buildZipBytes(t, map[string]string{
		"manifest.yaml": "id: " + id + "\nname: T\ndirect_egress: true\nentrypoints:\n  probe: probe\n",
		"init.lua":      "function probe() return {} end",
	})
	rec := postLuaPluginZip(t, "brandnew.zip", zipBytes)
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin(id) })

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"update":"true"`)) {
		t.Errorf("response = %s; a fresh install must not report update:true", rec.Body.String())
	}
	if !engine.LuaPlugins.Has(id) {
		t.Errorf("plugin %q not registered after fresh install", id)
	}
}
