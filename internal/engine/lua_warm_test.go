package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// warm_resolve injects the synthetic warm_resolve toggle, on by default; a
// manifest without it gets nothing.
func TestReadLuaManifest_WarmResolveInjection(t *testing.T) {
	mf, err := ReadLuaManifest(writeManifest(t, "id: p\nname: P\nwarm_resolve: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !mf.WarmResolve || !hasField(mf.Settings.Global, WarmResolveSettingID) {
		t.Fatalf("warm_resolve: expected the %q setting", WarmResolveSettingID)
	}
	for _, f := range mf.Settings.Global {
		if f.ID == WarmResolveSettingID && (f.Type != "bool" || f.Default != "true") {
			t.Fatalf("warm_resolve field = %+v, want a bool defaulting to true", f)
		}
	}
	plain, err := ReadLuaManifest(writeManifest(t, "id: p\nname: P\n"))
	if err != nil {
		t.Fatal(err)
	}
	if hasField(plain.Settings.Global, WarmResolveSettingID) {
		t.Fatal("warm_resolve injected without the manifest flag")
	}
}

// The background call passes its args and progress through, and with every
// background slot taken gives up after acquireWait with ErrPluginBusy
// instead of queuing.
func TestCallEntrypointJSONBackgroundCtx(t *testing.T) {
	dir := writeTestPlugin(t,
		"id: t.warm\nname: T\ndirect_egress: true\npool_size: 2\nentrypoints:\n  resolve: resolve_stream\n",
		`function resolve_stream(args)
		   mycelium.progress("", "sniff")
		   return { warm = args.warm == true, id = args.stream_id }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	var progress []string
	raw, err := m.CallEntrypointJSONBackgroundCtx(context.Background(), "t.warm", EPResolveStream,
		map[string]any{"stream_id": "s", "warm": true}, "", func(_, msg string) { progress = append(progress, msg) }, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Warm bool
		ID   string
	}
	if err := json.Unmarshal(raw, &out); err != nil || !out.Warm || out.ID != "s" {
		t.Fatalf("result %s (%v)", raw, err)
	}
	if len(progress) != 1 || progress[0] != "sniff" {
		t.Fatalf("progress = %v", progress)
	}

	m.mu.RLock()
	p := m.plugins["t.warm"]
	m.mu.RUnlock()
	free, err := p.Pool.BackgroundSlot(context.Background()) // the only one
	if err != nil {
		t.Fatal(err)
	}
	defer free()
	start := time.Now()
	_, err = m.CallEntrypointJSONBackgroundCtx(context.Background(), "t.warm", EPResolveStream,
		map[string]any{"stream_id": "s"}, "", nil, 50*time.Millisecond)
	if !errors.Is(err, ErrPluginBusy) {
		t.Fatalf("err = %v, want ErrPluginBusy", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("gave up after %s, want ~50ms", d)
	}
}
