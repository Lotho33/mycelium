package pileus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mycelium/internal/engine"
)

// loadContractPlugin installs a throwaway Lua plugin into the global engine
// registry (the pipeline calls engine.LuaPlugins directly).
func loadContractPlugin(t *testing.T, id, src string) *luaPipelineProvider {
	t.Helper()
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "id: " + id + "\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n" +
		"  streams: get_streams\n  resolve: resolve_stream\n  catalog: get_catalog\n  browse: browse\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init.lua"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := engine.LuaPlugins.LoadPlugin(dir); err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin(id) })
	return &luaPipelineProvider{id: id}
}

func TestLuaPipeline_Contract(t *testing.T) {
	p := loadContractPlugin(t, "t.contract", `
		function get_streams(args)
			if args.media_id == "none" then return { sources = {} } end
			if args.media_id == "legacy" then return nil end
			return { sources = { { id = "s1", label = "A" } } }
		end
		function resolve_stream(args)
			if args.stream_id == "missing" then return nil, "Doppiaggio italiano non disponibile" end
			return { url = "http://x/playlist.m3u8", headers = {}, extra = {} }
		end
		function get_catalog(args)
			return { items = { { id = "a", title = "A" } }, has_more = false }
		end
		function browse(args) return { { id = "e", title = "E" } } end
	`)
	ctx := context.Background()

	// An explicit empty list stays empty — no invented source.
	if srcs, err := p.GetStreams(ctx, "none"); err != nil || len(srcs) != 0 {
		t.Errorf("GetStreams(empty) = %v, %v; want no sources", srcs, err)
	}
	// nil keeps the legacy single-source behaviour.
	if srcs, err := p.GetStreams(ctx, "legacy"); err != nil || len(srcs) != 1 || srcs[0].Id != "legacy" {
		t.Errorf("GetStreams(nil) = %v, %v; want the legacy single source", srcs, err)
	}

	// Empty headers/extra tables serialise as [] and must not break resolve.
	if u, _, _, _, err := p.ResolveStream(ctx, "ok", nil); err != nil || u == "" {
		t.Errorf("ResolveStream with empty tables = %q, %v; want success", u, err)
	}

	// A plugin-described failure reaches the player verbatim, not as Internal.
	_, _, _, _, err := p.ResolveStream(ctx, "missing", nil)
	st, _ := status.FromError(wrapInternal(err))
	if st.Code() != codes.FailedPrecondition || st.Message() != "Doppiaggio italiano non disponibile" {
		t.Errorf("plugin error → %v %q; want FailedPrecondition with the plugin's message", st.Code(), st.Message())
	}

	// The plugin's has_more is believed; legacy arrays keep the old guess.
	if _, more, err := p.GetCatalog(ctx, "c", 1); err != nil || more {
		t.Errorf("GetCatalog has_more = %v, %v; want false (plugin said so)", more, err)
	}
	if resp, err := p.Browse(ctx, "d", 1); err != nil || !resp.HasMore {
		t.Errorf("Browse(plain array) has_more = %v, %v; want the legacy true", resp.GetHasMore(), err)
	}
}

func TestWrapInternal_Codes(t *testing.T) {
	cases := []struct {
		err  error
		want codes.Code
	}{
		{errors.Join(errors.New("x"), engine.ErrPluginTimeout), codes.DeadlineExceeded},
		{errors.Join(errors.New("x"), engine.ErrPluginBusy), codes.Unavailable},
		{engine.ErrPluginNotLoaded, codes.NotFound},
		{&engine.PluginError{PluginID: "p", Fn: "f", Msg: "m"}, codes.FailedPrecondition},
		{errors.New("boom"), codes.Internal},
	}
	for _, c := range cases {
		if got := status.Code(wrapInternal(c.err)); got != c.want {
			t.Errorf("wrapInternal(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
