package engine

import (
	"context"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// callScope holds the per-call state the mycelium.* SDK reads while an
// entrypoint runs. One lives in each pooled *lua.LState, so the SDK modules
// are built once (RegisterSDK). A single goroutine owns an LState between
// Acquire and release, so no locking. The callers fill it before the call
// and reset it on the clean return path; after a timeout the state is
// discarded and the scope left as-is (the abandoned goroutine may still read
// requireProxy).
type callScope struct {
	profileID string
	// requireProxy: this call must not go direct. proxyURL is its egress proxy
	// ("" with requireProxy means the egress is unavailable: the SDK blocks).
	requireProxy bool
	proxyURL     string
	onProgress   ProgressFunc
	// ctx is the running call's bounded context. Blocking SDK calls without a
	// timeout of their own (mycelium.sleep) select on it. nil outside a call.
	ctx context.Context
	// globalBaseline is the set of _G keys right after the top-level script ran
	// (snapshotGlobalBaseline). Not per-call state: reset() never touches it.
	globalBaseline map[string]struct{}
}

func (s *callScope) set(ctx context.Context, profileID string, requireProxy bool, proxyURL string, onProgress ProgressFunc) {
	s.ctx = ctx
	s.profileID = profileID
	s.requireProxy = requireProxy
	s.proxyURL = proxyURL
	s.onProgress = onProgress
}

// parentCtx is the context SDK I/O derives from: the running call's, so a
// cancelled request also aborts the I/O in flight. Background outside a call.
func (s *callScope) parentCtx() context.Context {
	if s != nil && s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// maxSDKTimeout caps a plugin-supplied timeout_seconds for a single SDK I/O.
const maxSDKTimeout = 90 * time.Second

// clampSDKTimeout converts a plugin-supplied timeout (seconds) to a
// duration in (0, maxSDKTimeout].
func clampSDKTimeout(sec int) time.Duration {
	d := time.Duration(sec) * time.Second
	if d <= 0 || d > maxSDKTimeout {
		return maxSDKTimeout
	}
	return d
}

func (s *callScope) reset() {
	s.ctx = nil
	s.profileID = ""
	s.requireProxy = false
	s.proxyURL = ""
	s.onProgress = nil
	// globalBaseline lives as long as the LState.
}

// snapshotGlobalBaseline records the current global keys as this state's
// baseline, once, right after init.lua ran (LuaPool.newState). Reads the
// globals through GlobalsIndex, so reassigning _G doesn't matter.
func (s *callScope) snapshotGlobalBaseline(L *lua.LState) {
	g, ok := L.Get(lua.GlobalsIndex).(*lua.LTable)
	if !ok {
		return
	}
	baseline := make(map[string]struct{})
	g.ForEach(func(k, _ lua.LValue) {
		if ks, ok := k.(lua.LString); ok {
			baseline[string(ks)] = struct{}{}
		}
	})
	s.globalBaseline = baseline
}

// resetNewGlobals deletes every global key not in the baseline, after each
// call that didn't time out. The same LState serves later calls, possibly
// of another profile: a value stashed in a global must not outlive the call
// that wrote it.
func (s *callScope) resetNewGlobals(L *lua.LState) {
	if s.globalBaseline == nil {
		// No baseline (a bare LState in tests): skip.
		return
	}
	g, ok := L.Get(lua.GlobalsIndex).(*lua.LTable)
	if !ok {
		return
	}
	var stale []string
	g.ForEach(func(k, _ lua.LValue) {
		ks, ok := k.(lua.LString)
		if !ok {
			return
		}
		if _, known := s.globalBaseline[string(ks)]; !known {
			stale = append(stale, string(ks))
		}
	})
	// Delete after the traversal, never during it.
	for _, k := range stale {
		L.SetGlobal(k, lua.LNil)
	}
}

// The scope pointer lives in the Lua registry (unreachable from Lua code)
// and goes away with the LState.
const luaScopeRegKey = "__mycelium_call_scope"

func attachScope(L *lua.LState, s *callScope) {
	ud := L.NewUserData()
	ud.Value = s
	L.SetField(L.Get(lua.RegistryIndex), luaScopeRegKey, ud)
}

// scopeOf returns the LState's callScope, or an empty one for a bare state.
func scopeOf(L *lua.LState) *callScope {
	if ud, ok := L.GetField(L.Get(lua.RegistryIndex), luaScopeRegKey).(*lua.LUserData); ok {
		if s, ok := ud.Value.(*callScope); ok {
			return s
		}
	}
	return &callScope{}
}
