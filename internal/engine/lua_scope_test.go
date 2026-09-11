package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// newQuietHTTPServer starts a 204-returning test server and returns its URL.
func newQuietHTTPServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeTestPlugin drops a manifest.yaml + init.lua into a temp dir and returns it.
func writeTestPlugin(t *testing.T, manifest, initLua string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init.lua"), []byte(initLua), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newTestManager(t *testing.T) *LuaPluginManager {
	t.Helper()
	m := &LuaPluginManager{
		plugins:      make(map[string]*LuaPlugin),
		directClient: &http.Client{},
	}
	t.Cleanup(m.Shutdown)
	return m
}

// The SDK module tables (mycelium, .context, .network, .browser) must be built
// once per LState and reused — never rebuilt per entrypoint call.
func TestSDKScope_ModulesBuiltOnce(t *testing.T) {
	dir := writeTestPlugin(t,
		"id: t.once\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n  probe: probe\n",
		`function probe(args)
		   return {
		     mycelium = tostring(mycelium),
		     context  = tostring(mycelium.context),
		     network  = tostring(mycelium.network),
		     browser  = tostring(mycelium.browser),
		   }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	call := func() map[string]string {
		raw, err := m.CallEntrypointJSON("t.once", "probe", nil, "")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		var out map[string]string
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		return out
	}

	a, b := call(), call()
	for _, k := range []string{"mycelium", "context", "network", "browser"} {
		if a[k] == "" {
			t.Fatalf("%s identity empty", k)
		}
		if a[k] != b[k] {
			t.Errorf("%s rebuilt between calls: %q → %q", k, a[k], b[k])
		}
	}
}

// profile id must reflect the CURRENT call, on a pool of size 1 where the same
// LState is reused — and reset to "" between calls (no bleed from the previous
// caller's profile).
func TestSDKScope_ProfileIDPerCallNoBleed(t *testing.T) {
	dir := writeTestPlugin(t,
		"id: t.prof\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n  probe: probe\n",
		`function probe(args)
		   return { fn = mycelium.context.get_profile_id(), field = mycelium.context.profile_id }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	probe := func(profileID string) (fn, field string) {
		raw, err := m.CallEntrypointJSON("t.prof", "probe", nil, profileID)
		if err != nil {
			t.Fatalf("call(%q): %v", profileID, err)
		}
		var out struct{ Fn, Field string }
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		return out.Fn, out.Field
	}

	if fn, field := probe("alice"); fn != "alice" || field != "alice" {
		t.Errorf("profile alice: get_profile_id()=%q profile_id=%q", fn, field)
	}
	if fn, field := probe("bob"); fn != "bob" || field != "bob" {
		t.Errorf("profile bob: get_profile_id()=%q profile_id=%q", fn, field)
	}
	// A task-style call with no profile — must not see "bob" left over.
	if fn, field := probe(""); fn != "" || field != "" {
		t.Errorf("empty profile bled previous value: get_profile_id()=%q profile_id=%q", fn, field)
	}
}

// requireProxy is per-call: a VPN-routed plugin with no proxy configured must
// fail its network calls closed, and that decision is read live from the scope
// (not baked at module-build time).
func TestSDKScope_RequireProxyFailsClosed(t *testing.T) {
	dir := writeTestPlugin(t,
		// no direct_egress → VPN-routed → requireProxy true; manager has no proxyClient
		"id: t.vpn\nname: T\npool_size: 1\nentrypoints:\n  probe: probe\n",
		`function probe(args)
		   local r = mycelium.network.get("http://198.51.100.1/should-not-be-reached")
		   return { status = r.status_code, err = r.error or "" }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	raw, err := m.CallEntrypointJSON("t.vpn", "probe", nil, "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out struct {
		Status int    `json:"status"`
		Err    string `json:"err"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if out.Status != 0 || out.Err == "" {
		t.Fatalf("VPN-routed call with no proxy should fail closed, got status=%d err=%q", out.Status, out.Err)
	}
}

func TestIconPath(t *testing.T) {
	const initLua = "function probe() return {} end"

	t.Run("valid", func(t *testing.T) {
		dir := writeTestPlugin(t, "id: ic.ok\nname: T\ndirect_egress: true\nicon: brand/icon.svg\nentrypoints:\n  probe: probe\n", initLua)
		if err := os.MkdirAll(filepath.Join(dir, "brand"), 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(dir, "brand", "icon.svg")
		if err := os.WriteFile(want, []byte("<svg/>"), 0o644); err != nil {
			t.Fatal(err)
		}
		m := newTestManager(t)
		if err := m.loadPlugin(dir); err != nil {
			t.Fatal(err)
		}
		got, ok := m.IconPath("ic.ok")
		if !ok || got != want {
			t.Fatalf("IconPath = %q,%v; want %q,true", got, ok, want)
		}
	})

	t.Run("no icon declared", func(t *testing.T) {
		dir := writeTestPlugin(t, "id: ic.none\nname: T\ndirect_egress: true\nentrypoints:\n  probe: probe\n", initLua)
		m := newTestManager(t)
		if err := m.loadPlugin(dir); err != nil {
			t.Fatal(err)
		}
		if _, ok := m.IconPath("ic.none"); ok {
			t.Fatal("IconPath ok for a plugin with no icon: field")
		}
	})

	t.Run("file missing", func(t *testing.T) {
		dir := writeTestPlugin(t, "id: ic.missing\nname: T\ndirect_egress: true\nicon: icon.svg\nentrypoints:\n  probe: probe\n", initLua)
		m := newTestManager(t)
		if err := m.loadPlugin(dir); err != nil {
			t.Fatal(err)
		}
		if _, ok := m.IconPath("ic.missing"); ok {
			t.Fatal("IconPath ok for a declared-but-absent icon file")
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		dir := writeTestPlugin(t, "id: ic.trav\nname: T\ndirect_egress: true\nicon: ../../../../etc/hosts\nentrypoints:\n  probe: probe\n", initLua)
		m := newTestManager(t)
		if err := m.loadPlugin(dir); err != nil {
			t.Fatal(err)
		}
		if got, ok := m.IconPath("ic.trav"); ok {
			t.Fatalf("IconPath allowed a traversal path: %q", got)
		}
	})

	t.Run("unknown plugin", func(t *testing.T) {
		m := newTestManager(t)
		if _, ok := m.IconPath("nope"); ok {
			t.Fatal("IconPath ok for an unknown plugin")
		}
	})
}

// A direct_egress plugin on the same manager (no proxy) must NOT be blocked —
// proves requireProxy is derived per plugin/call, not a global.
func TestSDKScope_DirectEgressNotBlocked(t *testing.T) {
	// Point the request at a local listener so a "reached the network" result
	// is deterministic and offline-safe.
	srv := newQuietHTTPServer(t)
	dir := writeTestPlugin(t,
		"id: t.direct\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n  probe: probe\n",
		`function probe(args)
		   local r = mycelium.network.get(args.url)
		   return { status = r.status_code, err = r.error or "" }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	raw, err := m.CallEntrypointJSON("t.direct", "probe", map[string]any{"url": srv}, "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out struct {
		Status int    `json:"status"`
		Err    string `json:"err"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if out.Status != 204 {
		t.Fatalf("direct_egress call should have reached the local server (204), got status=%d err=%q", out.Status, out.Err)
	}
}
