package engine

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// mycelium.cache.remember(key, ttl, fn): the "read the cache, compute on a
// miss, store" pattern every plugin wrote by hand, plus request coalescing:
// while one state computes a key, the other states of the plugin asking for
// the same key wait for its result instead of repeating the work (two VMs
// warming the same metadata lookup, a carousel and a detail page at once).

// rememberRedisTimeout bounds the cache read/write around fn.
const rememberRedisTimeout = 2 * time.Second

type rememberFlight struct {
	done chan struct{}
	val  []byte // JSON of the result; nil = the leader failed
}

type rememberGroup struct {
	mu sync.Mutex
	m  map[string]*rememberFlight
}

var rememberFlights = &rememberGroup{m: map[string]*rememberFlight{}}

// join returns the flight for key and whether the caller leads it.
func (g *rememberGroup) join(key string) (*rememberFlight, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f := g.m[key]; f != nil {
		return f, false
	}
	f := &rememberFlight{done: make(chan struct{})}
	g.m[key] = f
	return f, true
}

func (g *rememberGroup) finish(key string, f *rememberFlight, val []byte) {
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	f.val = val
	close(f.done)
}

// decodeRemembered turns stored JSON back into a Lua value.
func decodeRemembered(L *lua.LState, raw []byte) (lua.LValue, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return lua.LNil, false
	}
	return goToLua(L, v), true
}

// cacheRememberFunc is mycelium.cache.remember(key, ttl, fn) → value, hit.
// fn() returns the value (table, string, number, bool; nil = nothing to
// store) and optionally a ttl overriding the default for this result (e.g.
// shorter for an empty one). hit is true when the value came from the cache
// or from another state's computation.
func cacheRememberFunc(opts SDKOpts, scope *callScope, fullKey func(string) string) lua.LGFunction {
	return func(L *lua.LState) int {
		key := L.CheckString(1)
		ttl := L.CheckInt(2)
		fn := L.CheckFunction(3)
		rk := fullKey(key)

		redisGet := func() ([]byte, bool) {
			if opts.Redis == nil {
				return nil, false
			}
			ctx, cancel := context.WithTimeout(scope.parentCtx(), rememberRedisTimeout)
			defer cancel()
			s, err := opts.Redis.Get(ctx, rk)
			if err != nil || s == "" {
				return nil, false
			}
			return []byte(s), true
		}

		if raw, ok := redisGet(); ok {
			if v, ok := decodeRemembered(L, raw); ok {
				L.Push(v)
				L.Push(lua.LTrue)
				return 2
			}
		}

		f, leader := rememberFlights.join(rk)
		if !leader {
			select {
			case <-f.done:
				if f.val != nil {
					if v, ok := decodeRemembered(L, f.val); ok {
						L.Push(v)
						L.Push(lua.LTrue)
						return 2
					}
				}
			case <-scope.parentCtx().Done():
				L.RaiseError("cache.remember %q: call ended while waiting", key)
			}
			// The leader failed: compute here, without coalescing.
			L.Push(fn)
			L.Call(0, 1)
			L.Push(lua.LFalse)
			return 2
		}

		var result []byte
		defer func() { rememberFlights.finish(rk, f, result) }()
		L.Push(fn)
		if err := L.PCall(0, 2, nil); err != nil {
			L.RaiseError("%s", err.Error())
		}
		val, ttlOverride := L.Get(-2), L.Get(-1)
		L.Pop(2)
		if n, ok := ttlOverride.(lua.LNumber); ok {
			ttl = int(n)
		}
		raw, err := json.Marshal(luaToJSON(val))
		if err != nil {
			raw = []byte("null")
		}
		result = raw
		if val != lua.LNil && ttl > 0 && opts.Redis != nil && len(raw) <= maxCacheValueBytes {
			ctx, cancel := context.WithTimeout(scope.parentCtx(), rememberRedisTimeout)
			_ = opts.Redis.Set(ctx, rk, string(raw), time.Duration(ttl)*time.Second)
			cancel()
		}
		L.Push(val)
		L.Push(lua.LFalse)
		return 2
	}
}
