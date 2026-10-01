package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// After a timeout the abandoned goroutine must actually stop: callWithTimeout
// binds ctx to the VM, so a pure-Lua infinite loop is interrupted instead of
// burning a core forever.
func TestCallWithTimeout_StopsAbandonedInfiniteLoop(t *testing.T) {
	// Not closed: like production (LuaPool.DiscardAndReplace), a timed-out
	// state is abandoned, never touched again by the caller.
	L := lua.NewState()

	var ticks atomic.Int64
	L.SetGlobal("tick", L.NewFunction(func(L *lua.LState) int { ticks.Add(1); return 0 }))
	if err := L.DoString(`function spin() while true do tick() end end`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, timedOut := callWithTimeout(ctx, L, L.GetGlobal("spin"), L.NewTable())
	if !timedOut {
		t.Fatal("expected a timeout")
	}

	// Give the VM a moment to notice ctx.Done, then the counter must freeze.
	time.Sleep(50 * time.Millisecond)
	before := ticks.Load()
	time.Sleep(100 * time.Millisecond)
	if after := ticks.Load(); after != before {
		t.Fatalf("abandoned Lua loop still running after timeout (%d → %d ticks)", before, after)
	}
}

// On normal completion the context is detached again, so the pooled state
// runs its next call without a stale (possibly cancelled) context.
func TestCallWithTimeout_ClearsContextOnSuccess(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	if err := L.DoString(`function ok() return 1 end`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err, timedOut := callWithTimeout(ctx, L, L.GetGlobal("ok"), L.NewTable()); err != nil || timedOut {
		t.Fatalf("call: err=%v timedOut=%v", err, timedOut)
	}
	cancel()
	if L.Context() != nil {
		t.Fatal("context still bound to the LState after a successful call")
	}
	if err := L.DoString(`local x = 0 for i = 1, 1000 do x = x + i end`); err != nil {
		t.Fatalf("state unusable after a cancelled context: %v", err)
	}
}

// A self-referencing or pathological table returned/serialised by a plugin
// must not crash the process (Go stack overflow is fatal) nor allocate
// unboundedly.
func TestLuaConverters_SurvivePathologicalTables(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	if err := L.DoString(`
		cyc = { name = "x" }
		cyc.self = cyc
		cyc.list = { cyc, cyc }
		sparse = {}
		sparse[1000000000] = 1
	`); err != nil {
		t.Fatal(err)
	}
	cyc := L.GetGlobal("cyc")
	sparse := L.GetGlobal("sparse")

	if m, ok := luaToGo(cyc).(map[string]any); !ok || m["name"] != "x" || m["self"] != nil {
		t.Errorf("luaToGo(cyclic) = %#v, want name kept and the back-reference nil", luaToGo(cyc))
	}
	if m, ok := luaToJSON(cyc).(map[string]any); !ok || m["self"] != nil {
		t.Errorf("luaToJSON(cyclic) = %#v, want the back-reference nil", luaToJSON(cyc))
	}
	var buf bytes.Buffer
	luaValueToJSON(&buf, cyc)
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("luaValueToJSON(cyclic) produced invalid JSON %q: %v", buf.String(), err)
	}
	if _, isSlice := luaToJSON(sparse).([]interface{}); isSlice {
		t.Error("luaToJSON(sparse t[1e9]) built an array — must fall back to an object")
	}
}

// A caller that goes away (request cancelled) must stop the plugin call —
// not leave it holding a pool slot for its whole entrypoint budget.
func TestCallEntrypointJSONCtx_CancelStopsCall(t *testing.T) {
	dir := writeTestPlugin(t,
		"id: t.cancel\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n  probe: probe\n",
		`function probe(args) mycelium.sleep(20000) return {} end`)
	m := newTestManager(t)
	if err := m.loadPlugin(dir); err != nil {
		t.Fatalf("loadPlugin: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := m.CallEntrypointJSONCtx(ctx, "t.cancel", "probe", nil, "")
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("call returned after %v, want shortly after the 100ms cancel", el)
	}
}
