package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// mycelium.sleep(ms) must be cut short when the calling entrypoint's context
// expires, instead of blocking a (post-timeout, abandoned per callWithTimeout)
// goroutine — and the *lua.LState it pins — for the full requested duration.
// Regression test for the leak: before the fix this was a bare time.Sleep
// with no ctx at all.
func TestSDKSleep_InterruptedByEntrypointTimeout(t *testing.T) {
	dir := writeTestPlugin(t,
		"id: t.sleep\nname: T\ndirect_egress: true\npool_size: 1\n"+
			"entrypoints:\n  probe: probe\n"+
			"tasks:\n  - function: probe\n    timeout_seconds: 1\n",
		`function probe(args)
		   mycelium.sleep(10000)
		   mycelium.log("after-sleep")
		   return {}
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	baseline := runtime.NumGoroutine()
	start := time.Now()
	_, err := m.CallEntrypointJSON("t.sleep", "probe", nil, "")
	if err == nil {
		t.Fatal("expected a timeout error from CallEntrypointJSON (1s task budget, 10s requested sleep)")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("CallEntrypointJSON took %v, want ~1s (the task's timeout_seconds)", elapsed)
	}

	// The abandoned goroutine must not linger for the 10s the plugin asked
	// for: mycelium.sleep returns on the expired ctx, and the VM (bound to
	// the same ctx by callWithTimeout) aborts at the next instruction — so
	// "after-sleep" is never logged, and the goroutine count drops back.
	deadline := time.Now().Add(4 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("abandoned plugin goroutine still alive 4s after the timeout (goroutines %d > baseline %d) — mycelium.sleep(10000) is not respecting the entrypoint ctx", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if buf := m.GetLogBuffer("t.sleep"); buf != nil {
		for _, line := range buf.Lines() {
			if strings.Contains(line, "after-sleep") {
				t.Fatal("Lua kept executing after the entrypoint timeout (after-sleep logged)")
			}
		}
	}
}

// zeroReader is an infinite source of zero bytes, used to serve an
// oversized HTTP response body without allocating it in memory up front.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// mycelium.network.get must never read more than maxSDKResponseBytes of a
// response body into memory, however much the server offers.
func TestSDKNetworkGet_CapsResponseBodySize(t *testing.T) {
	const serveSize = int64(maxSDKResponseBytes) + (8 << 20) // deliberately over the cap

	var serverSentBytes int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n, _ := io.Copy(w, io.LimitReader(zeroReader{}, serveSize))
		atomic.StoreInt64(&serverSentBytes, n)
	}))
	t.Cleanup(srv.Close)

	dir := writeTestPlugin(t,
		"id: t.bigresp\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n  probe: probe\n",
		`function probe(args)
		   local r = mycelium.network.get(args.url)
		   return { len = #r.body, status = r.status_code, err = r.error or "" }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	raw, err := m.CallEntrypointJSON("t.bigresp", "probe", map[string]any{"url": srv.URL}, "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var out struct {
		Len    int    `json:"len"`
		Status int    `json:"status"`
		Err    string `json:"err"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if out.Status != 200 {
		t.Fatalf("status=%d err=%q", out.Status, out.Err)
	}
	if out.Len != maxSDKResponseBytes {
		t.Fatalf("network.get read %d bytes, want exactly the %d-byte cap (server offered %d)",
			out.Len, maxSDKResponseBytes, serveSize)
	}
	if got := atomic.LoadInt64(&serverSentBytes); got >= serveSize {
		t.Fatalf("server finished sending all %d bytes uninterrupted — response reading was never capped client-side", got)
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

// A plugin's global table must not leak state written during one entrypoint
// call into a later call served by the same pooled LState — pool_size
// defaults to 2, so consecutive calls can belong to different profiles/users.
// Regression test for bug 1 (2026-09-12 audit): before the fix, a value one
// call stashed in a global (a common ad-hoc "caching" pattern) was still
// visible to whichever call the pool handed that same LState to next.
func TestSDKScope_GlobalStateNotLeakedAcrossCalls(t *testing.T) {
	dir := writeTestPlugin(t,
		"id: t.globalleak\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n  probe: probe\n",
		`top_level_const = "baseline-value"

		 function probe(args)
		   local leaked = leaked_secret
		   leaked_secret = "profile-A-data"
		   return { leaked = leaked or "__nil__", const = top_level_const }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	probe := func(profileID string) (leaked, constVal string) {
		raw, err := m.CallEntrypointJSON("t.globalleak", "probe", nil, profileID)
		if err != nil {
			t.Fatalf("call(%q): %v", profileID, err)
		}
		var out struct{ Leaked, Const string }
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		return out.Leaked, out.Const
	}

	// First call ("profile A") — nothing set yet, and it writes leaked_secret.
	if leaked, cst := probe("profile-A"); leaked != "__nil__" || cst != "baseline-value" {
		t.Fatalf("first call: leaked=%q const=%q, want __nil__/baseline-value", leaked, cst)
	}
	// Second call ("profile B"), on the SAME pooled LState (pool_size: 1),
	// must NOT see what the first call wrote — that would leak profile A's
	// data into profile B's call. The top-level constant — part of the
	// legitimate load-time baseline — must still be intact.
	leaked, cst := probe("profile-B")
	if leaked != "__nil__" {
		t.Fatalf("second call saw leaked_secret=%q — global state leaked across calls/profiles", leaked)
	}
	if cst != "baseline-value" {
		t.Fatalf("second call: top-level const corrupted by the reset, got %q", cst)
	}
}

// Repeated entrypoint timeouts must increment the plugin's discarded-states
// counter (bug 2, 2026-09-12 audit) and that count must be readable from both
// the direct accessor and the struct the admin dashboard consumes — before
// the fix, an always-timing-out task/entrypoint leaked one goroutine + one
// *lua.LState per occurrence with no counter, cap, or log anywhere.
func TestLuaPlugin_DiscardedStatesCounted(t *testing.T) {
	dir := writeTestPlugin(t,
		"id: t.discard\nname: T\ndirect_egress: true\npool_size: 1\n"+
			"entrypoints:\n  probe: probe\n"+
			"tasks:\n  - function: probe\n    timeout_seconds: 1\n",
		`function probe(args)
		   mycelium.sleep(10000)
		   return {}
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	const attempts = 2
	for i := 0; i < attempts; i++ {
		if _, err := m.CallEntrypointJSON("t.discard", "probe", nil, ""); err == nil {
			t.Fatalf("attempt %d: expected a timeout error (1s task budget, 10s requested sleep)", i)
		}
	}

	if got := m.DiscardedStates("t.discard"); got != attempts {
		t.Fatalf("DiscardedStates(%q) = %d, want %d", "t.discard", got, attempts)
	}

	found := false
	for _, meta := range m.GetMetaWithStatus() {
		if meta.ID != "t.discard" {
			continue
		}
		found = true
		if meta.DiscardedStates != attempts {
			t.Fatalf("GetMetaWithStatus DiscardedStates = %d, want %d", meta.DiscardedStates, attempts)
		}
	}
	if !found {
		t.Fatal("t.discard not present in GetMetaWithStatus")
	}
}

// mycelium.cache.set must reject a value larger than maxCacheValueBytes
// instead of writing it — without this cap a plugin can grow the shared
// Redis instance without bound (cache.set also allows ttl=0, i.e. no
// expiry). Regression test for bug 2 (2026-09-12 audit).
func TestSDKCacheSet_CapsValueSize(t *testing.T) {
	dir := writeTestPlugin(t,
		"id: t.cachecap\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n  probe: probe\n",
		`function probe(args)
		   local val = string.rep("a", args.size)
		   local ok, err = mycelium.cache.set("k", val, 0)
		   return { ok = ok, err = err or "" }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	call := func(size int) (ok bool, errMsg string) {
		raw, err := m.CallEntrypointJSON("t.cachecap", "probe", map[string]any{"size": size}, "")
		if err != nil {
			t.Fatalf("call(size=%d): %v", size, err)
		}
		var out struct {
			OK  bool   `json:"ok"`
			Err string `json:"err"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		return out.OK, out.Err
	}

	t.Run("over cap fails with a readable error", func(t *testing.T) {
		ok, errMsg := call(maxCacheValueBytes + 1)
		if ok {
			t.Fatal("cache.set(oversized value) returned ok=true, want a rejected write")
		}
		if !strings.Contains(errMsg, "too large") {
			t.Fatalf("cache.set(oversized value) err = %q, want it to mention the size cap", errMsg)
		}
	})

	t.Run("under cap is not rejected by the size cap", func(t *testing.T) {
		// No real Redis is wired up in this unit test (opts.Redis is nil), so
		// a within-cap write still fails — but with "redis unavailable", not
		// the size-cap error, proving the value cleared the cap check and
		// reached the actual Redis call. That's the "no regression" signal
		// this test can give without a live Redis instance.
		ok, errMsg := call(1024)
		if ok {
			t.Fatal("cache.set unexpectedly succeeded with no Redis configured in the test manager")
		}
		if strings.Contains(errMsg, "too large") {
			t.Fatalf("cache.set(1KiB value) was rejected by the size cap: %q", errMsg)
		}
		if errMsg != "redis unavailable" {
			t.Fatalf("cache.set(1KiB value) err = %q, want %q", errMsg, "redis unavailable")
		}
	})
}

// mycelium.storage.write_json must reject a payload larger than
// maxStorageWriteBytes instead of writing it to disk — without this cap a
// plugin can fill its own plugin directory (and, in aggregate, the host
// disk) with unbounded writes. Regression test for bug 2 (2026-09-12 audit).
func TestSDKStorageWriteJSON_CapsPayloadSize(t *testing.T) {
	dir := writeTestPlugin(t,
		"id: t.storagecap\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n  probe: probe\n",
		`function probe(args)
		   local err = mycelium.storage.write_json(args.name, { data = string.rep("a", args.size) })
		   return { err = err or "" }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}

	call := func(name string, size int) string {
		raw, err := m.CallEntrypointJSON("t.storagecap", "probe", map[string]any{"name": name, "size": size}, "")
		if err != nil {
			t.Fatalf("call(size=%d): %v", size, err)
		}
		var out struct {
			Err string `json:"err"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		return out.Err
	}

	t.Run("over cap fails with a readable error and writes nothing", func(t *testing.T) {
		const name = "over.json"
		errMsg := call(name, maxStorageWriteBytes+1)
		if !strings.Contains(errMsg, "too large") {
			t.Fatalf("write_json(oversized payload) err = %q, want it to mention the size cap", errMsg)
		}
		if _, statErr := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(statErr) {
			t.Fatalf("write_json(oversized payload) left a file behind (stat err = %v), want no file written", statErr)
		}
	})

	t.Run("under cap writes normally", func(t *testing.T) {
		const name = "under.json"
		errMsg := call(name, 1024)
		if errMsg != "" {
			t.Fatalf("write_json(1KiB payload) err = %q, want no error", errMsg)
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("expected %s to be written: %v", name, err)
		}
		var got struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal written file: %v", err)
		}
		if len(got.Data) != 1024 {
			t.Fatalf("written data field has length %d, want 1024", len(got.Data))
		}
	})
}

// write_json must not reach the plugin's own code or manifest.yaml: JSON is
// valid YAML, so a rewritten manifest could grant direct_egress or claim
// another plugin's id on the next load.
func TestSDKStorageWriteJSON_OnlyJSONFiles(t *testing.T) {
	manifest := "id: t.storageext\nname: T\ndirect_egress: false\npool_size: 1\nentrypoints:\n  probe: probe\n"
	dir := writeTestPlugin(t, manifest,
		`function probe(args)
		   local err = mycelium.storage.write_json(args.name, { id = "hijack", direct_egress = true })
		   return { err = err or "" }
		 end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}
	for _, name := range []string{"manifest.yaml", "init.lua", "MANIFEST.YAML", "sub/../manifest.yaml"} {
		raw, err := m.CallEntrypointJSON("t.storageext", "probe", map[string]any{"name": name}, "")
		if err != nil {
			t.Fatalf("call(%s): %v", name, err)
		}
		if !strings.Contains(string(raw), "only .json") {
			t.Errorf("write_json(%q) = %s, want an 'only .json' rejection", name, raw)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil || string(got) != manifest {
		t.Fatalf("manifest.yaml was modified: %q (err %v)", got, err)
	}
}
