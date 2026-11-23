package pileus

import (
	"os"
	"path/filepath"
	"testing"

	"mycelium/internal/engine"
)

func TestParseLuaResolved_ProxyFlag(t *testing.T) {
	rs, err := parseLuaResolved([]byte(`{"url":"http://lan:8096/master.m3u8","proxy":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Proxy {
		t.Fatal("proxy = true not read")
	}
	rs, err = parseLuaResolved([]byte(`{"url":"http://lan:8096/file.mkv"}`))
	if err != nil {
		t.Fatal(err)
	}
	if rs.Proxy {
		t.Fatal("proxy defaults to false")
	}
}

func TestSkipProxy_ResolveOverridesDirectStream(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "t.direct")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "id: t.direct\nname: T\ndirect_egress: true\ndirect_stream: true\npool_size: 1\n" +
		"entrypoints:\n  resolve: resolve_stream\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init.lua"), []byte("function resolve_stream() end"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := engine.LuaPlugins.LoadPlugin(dir); err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin("t.direct") })

	if !skipProxy("t.direct", &ResolvedStream{}) {
		t.Fatal("direct_stream plugin went through the proxy")
	}
	if skipProxy("t.direct", &ResolvedStream{Proxy: true}) {
		t.Fatal("proxy = true ignored for a direct_stream plugin")
	}
	if skipProxy("nope", &ResolvedStream{}) {
		t.Fatal("unknown plugin skipped the proxy")
	}
}
