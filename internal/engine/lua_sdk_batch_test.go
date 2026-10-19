package engine

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// sdkTestState is a bare LState with network/cache/ratelimit for plugin id.
func sdkTestState(t *testing.T, id string) *lua.LState {
	t.Helper()
	L := lua.NewState()
	t.Cleanup(L.Close)
	opts := SDKOpts{PluginID: id, HTTPClient: http.DefaultClient}
	scope := &callScope{}
	m := L.NewTable()
	m.RawSetString("network", buildNetworkModule(L, opts, scope))
	m.RawSetString("cache", buildCacheModule(L, opts, scope))
	m.RawSetString("ratelimit", buildRatelimitModule(L, opts, scope))
	L.SetGlobal("mycelium", m)
	return L
}

func TestGetMany_ParallelAndOrdered(t *testing.T) {
	var inFlight, peak int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Echo", r.Header.Get("X-Test"))
		if r.URL.Path == "/404" {
			w.WriteHeader(404)
		}
		io.WriteString(w, r.Method+" "+r.URL.Path+" "+string(body))
	}))
	defer srv.Close()

	L := sdkTestState(t, "batch")
	L.SetGlobal("BASE", lua.LString(srv.URL))
	start := time.Now()
	runLua(t, L, `
		local reqs = {}
		for i = 1, 8 do reqs[i] = BASE .. "/p" .. i end
		reqs[9] = { url = BASE .. "/post", method = "post", body = "ciao", headers = { ["X-Test"] = "h" } }
		reqs[10] = { url = BASE .. "/404" }
		reqs[11] = { method = "GET" }
		local rs = mycelium.network.get_many(reqs, { concurrency = 16 })
		assert(#rs == 11)
		for i = 1, 8 do assert(rs[i].status_code == 200 and rs[i].body == "GET /p" .. i .. " ", rs[i].body) end
		assert(rs[9].body == "POST /post ciao" and rs[9].headers["X-Echo"] == "h")
		assert(rs[10].status_code == 404)
		assert(rs[11].status_code == 0 and rs[11].error == "missing url")
	`)
	if took := time.Since(start); took > 900*time.Millisecond {
		t.Fatalf("10 requests of 150ms took %v: not parallel", took)
	}
	if p := atomic.LoadInt32(&peak); p < 5 {
		t.Fatalf("peak concurrency %d", p)
	}

	// concurrency caps the parallel requests.
	atomic.StoreInt32(&peak, 0)
	runLua(t, L, `
		local reqs = {}
		for i = 1, 6 do reqs[i] = BASE .. "/c" .. i end
		mycelium.network.get_many(reqs, { concurrency = 2 })
	`)
	if p := atomic.LoadInt32(&peak); p > 2 {
		t.Fatalf("concurrency=2 but peak %d", p)
	}

	err := L.DoString(`local r = {} for i = 1, 33 do r[i] = BASE end mycelium.network.get_many(r)`)
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("err = %v", err)
	}
}

func TestRatelimit_SharedSpacingAndPause(t *testing.T) {
	t.Cleanup(func() { pluginLimiters.dropPlugin("rl") })
	a := sdkTestState(t, "rl")
	b := sdkTestState(t, "rl")
	other := sdkTestState(t, "rl.other")

	runLua(t, a, `mycelium.ratelimit.set("svc", 20, 1)`)
	start := time.Now()
	// 1 burst token + 4 more at 20/s = ~200ms, spread over two states.
	runLua(t, a, `for i = 1, 3 do assert(mycelium.ratelimit.wait("svc")) end`)
	runLua(t, b, `for i = 1, 2 do assert(mycelium.ratelimit.wait("svc")) end`)
	if took := time.Since(start); took < 180*time.Millisecond {
		t.Fatalf("5 waits at 20/s took %v", took)
	}
	// Unknown bucket (and another plugin's namespace) doesn't limit.
	start = time.Now()
	runLua(t, other, `for i = 1, 50 do assert(mycelium.ratelimit.wait("svc")) end`)
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Fatalf("unconfigured bucket waited %v", took)
	}
	// pause: a short max_wait gives up instead of sleeping.
	runLua(t, a, `
		mycelium.ratelimit.pause("svc", 5)
		assert(mycelium.ratelimit.wait("svc", 0.1) == false)
	`)
}

func TestCacheRemember_CoalescesAcrossStates(t *testing.T) {
	var calls int32
	gate := make(chan struct{})
	const n = 4
	states := make([]*lua.LState, n)
	for i := range states {
		L := sdkTestState(t, "rem")
		L.SetGlobal("COMPUTE", L.NewFunction(func(L *lua.LState) int {
			atomic.AddInt32(&calls, 1)
			<-gate
			t := L.NewTable()
			t.RawSetString("v", lua.LString("ok"))
			L.Push(t)
			return 1
		}))
		states[i] = L
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, L := range states {
		wg.Add(1)
		go func(i int, L *lua.LState) {
			defer wg.Done()
			errs[i] = L.DoString(`
				local v = mycelium.cache.remember("k", 60, function() return COMPUTE() end)
				assert(v.v == "ok")
			`)
		}(i, L)
	}
	time.Sleep(100 * time.Millisecond) // let everyone join the flight
	close(gate)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Fatalf("fn ran %d times, want 1", c)
	}

	// A failing leader: its error propagates, and a waiting state computes
	// on its own.
	L1, L2 := states[0], states[1]
	release := make(chan struct{})
	L1.SetGlobal("FAIL", L1.NewFunction(func(L *lua.LState) int {
		<-release
		L.RaiseError("boom")
		return 0
	}))
	done := make(chan error, 1)
	go func() {
		done <- L1.DoString(`mycelium.cache.remember("k2", 60, function() FAIL() end)`)
	}()
	time.Sleep(50 * time.Millisecond)
	go func() { time.Sleep(50 * time.Millisecond); close(release) }()
	runLua(t, L2, `
		local v, hit = mycelium.cache.remember("k2", 60, function() return "mine", 5 end)
		assert(v == "mine" and hit == false)
	`)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("leader err = %v", err)
	}
}
