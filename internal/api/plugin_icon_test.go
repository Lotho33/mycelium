package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mycelium/internal/engine"
)

func loadIconPlugin(t *testing.T, id, manifest string, iconBody []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init.lua"), []byte("function probe() return {} end"), 0o644); err != nil {
		t.Fatal(err)
	}
	if iconBody != nil {
		if err := os.WriteFile(filepath.Join(dir, "icon.svg"), iconBody, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.LuaPlugins.LoadPlugin(dir); err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin(id) })
}

func TestPluginIconRoute(t *testing.T) {
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"></svg>`
	loadIconPlugin(t, "icroute.ok",
		"id: icroute.ok\nname: T\ndirect_egress: true\nicon: icon.svg\nentrypoints:\n  probe: probe\n",
		[]byte(svg))
	loadIconPlugin(t, "icroute.noicon",
		"id: icroute.noicon\nname: T\ndirect_egress: true\nentrypoints:\n  probe: probe\n",
		nil)

	call := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/plugin-icon/"+id, nil)
		req.SetPathValue("plugin_id", id)
		rec := httptest.NewRecorder()
		pluginIcon(rec, req)
		return rec
	}

	t.Run("serves svg", func(t *testing.T) {
		rec := call("icroute.ok")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
			t.Errorf("Content-Type = %q; want image/svg+xml", ct)
		}
		if rec.Body.String() != svg {
			t.Errorf("body = %q; want the svg", rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") == "" {
			t.Errorf("no Cache-Control header")
		}
	})

	t.Run("404 when plugin has no icon", func(t *testing.T) {
		if rec := call("icroute.noicon"); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d; want 404", rec.Code)
		}
	})

	t.Run("404 for unknown plugin", func(t *testing.T) {
		if rec := call("does.not.exist"); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d; want 404", rec.Code)
		}
	})
}
