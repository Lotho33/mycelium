package engine

import (
	"context"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// callScope holds the per-entrypoint-call state the mycelium.* SDK modules
// need at call time. Exactly one lives per pooled *lua.LState, built once with
// the state and reused for every call that state serves — the SDK module
// tables and their closures are therefore built once (in RegisterSDK), not
// rebuilt on every GetCatalog/Search/Resolve like before.
//
// The Lua pool guarantees a single goroutine owns an LState between
// Acquire/release, so a scope is single-owner while a call runs — no locking.
// callEntrypoint{,JSON} fill it before invoking the plugin function; on the
// clean return path they reset it (via the same defer that returns the state
// to the pool). On a timeout the state is discarded, never reused, and the
// scope is deliberately left as-is: the abandoned goroutine may still be
// reading requireProxy, and flipping it to false under a VPN-routed call
// could drop that call onto a direct connection.
type callScope struct {
	profileID string
	// requireProxy: this call must NOT use a direct connection. proxyURL is
	// the resolved egress proxy for it ("" while requireProxy is true means
	// the selected egress is unavailable → the SDK blocks the call rather
	// than leaking direct).
	requireProxy bool
	proxyURL     string
	onProgress   ProgressFunc
	// ctx is the current entrypoint call's bounded context (deadline =
	// entrypointTimeout, see lua_plugin.go) — set by CallEntrypoint /
	// callEntrypointJSON before invoking the plugin function. Blocking SDK
	// calls that have no shorter timeout of their own (mycelium.sleep) select
	// on it so an abandoned goroutine (post-timeout, see callWithTimeout)
	// stops waiting instead of blocking for its full requested duration and
	// keeping the discarded *lua.LState alive indefinitely. nil outside a
	// call (e.g. a bare LState in tests) — callers must fall back to
	// context.Background() rather than dereference a nil context.
	ctx context.Context
	// globalBaseline is the set of _G keys present right after the plugin's
	// top-level script finished executing at LState creation (see
	// snapshotGlobalBaseline, called once from LuaPool.newState). It is NOT
	// per-call state — reset() must never touch it — it lives for the whole
	// lifetime of the LState and is used by resetNewGlobals to tell "the
	// plugin defined this at load time" (a top-level function, a
	// require()d module table, a config value read from the manifest) apart
	// from "a call wrote this into _G", which must not survive past that call.
	globalBaseline map[string]struct{}
}

func (s *callScope) set(ctx context.Context, profileID string, requireProxy bool, proxyURL string, onProgress ProgressFunc) {
	s.ctx = ctx
	s.profileID = profileID
	s.requireProxy = requireProxy
	s.proxyURL = proxyURL
	s.onProgress = onProgress
}

// parentCtx is the context SDK I/O (network, browser) should derive from:
// the running entrypoint's ctx, so a cancelled request or an expired
// entrypoint budget also aborts the HTTP/browser call in flight instead of
// letting it run on for its own full timeout. Background outside a call.
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
	// globalBaseline is deliberately NOT cleared here — it is captured once
	// at LState creation (snapshotGlobalBaseline) and must survive every
	// call this state serves for its whole lifetime, not just one.
}

// snapshotGlobalBaseline records the current keys of the Lua global table as
// this state's "legitimate" globals — called exactly once, from
// LuaPool.newState right after the plugin's init.lua has finished executing
// at the top level. Anything already in _G at that point (functions the
// script defines, require()d module tables, config values it read from the
// manifest at load time) is baseline; anything a later entrypoint call adds
// is not, and resetNewGlobals will strip it after that call.
//
// Reads the globals table via the GlobalsIndex pseudo-index rather than the
// `_G` global variable so this keeps working even if a script reassigns or
// clears `_G` itself.
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

// resetNewGlobals deletes every global-table key NOT present in the baseline
// snapshotted at load time (snapshotGlobalBaseline) — a best-effort cleanup
// run after every entrypoint call that completes without timing out (see
// CallEntrypoint/callEntrypointJSON in lua_plugin.go; a timed-out call skips
// this, same as it skips reset(), because the LState is discarded rather than
// reused — see LuaPool.DiscardAndReplace).
//
// Why this matters: pool_size defaults to 2, so the same *lua.LState is
// reacquired by later calls that can belong to a DIFFERENT profile/user. A
// plugin that stashes state in a global as a poor-man's cache between one
// entrypoint call and the next (e.g. `last_token = ...`) would otherwise leak
// that value to whichever caller's request happens to land on the same
// pooled state next — silently, until the next hot-reload discards it.
//
// This does NOT protect a global written DURING a call from being read by
// something else during that SAME call — there is no concurrency to guard
// against here, a single goroutine owns an LState between Acquire/release —
// it only prevents that global from surviving PAST the call that wrote it.
//
// Cost: one full pass over _G's keys per completed call. With the lockdown
// stdlib (lockdownStdlib in lua_pool.go) and a typical plugin script this is
// on the order of a few dozen keys, not thousands, so the extra pass is
// negligible next to the network I/O most entrypoints do; if a future plugin
// pattern ever pushes _G into the thousands of keys this would be worth
// revisiting, but that is not the shape of any plugin in this repo today.
func (s *callScope) resetNewGlobals(L *lua.LState) {
	if s.globalBaseline == nil {
		// No baseline captured (bare LState in a test, or attachScope/newState
		// ordering changed) — nothing safe to diff against, skip rather than
		// risk wiping out legitimate plugin state.
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
	// Delete after the ForEach completes rather than during it — mutating a
	// table mid-traversal is asking for trouble, and the extra allocation
	// here is a slice of a handful of strings at most.
	for _, k := range stale {
		L.SetGlobal(k, lua.LNil)
	}
}

// The scope pointer rides in the Lua registry — a table Lua code can't reach —
// so it travels with the LState and is GC'd with it. No manager-side
// map[*lua.LState]*callScope to keep in sync with pool Reload/Close.
const luaScopeRegKey = "__mycelium_call_scope"

func attachScope(L *lua.LState, s *callScope) {
	ud := L.NewUserData()
	ud.Value = s
	L.SetField(L.Get(lua.RegistryIndex), luaScopeRegKey, ud)
}

// scopeOf returns the LState's callScope. It is always present in states built
// by the pool; the fallback keeps the SDK safe rather than nil-panicking if a
// caller ever hands in a bare LState.
func scopeOf(L *lua.LState) *callScope {
	if ud, ok := L.GetField(L.Get(lua.RegistryIndex), luaScopeRegKey).(*lua.LUserData); ok {
		if s, ok := ud.Value.(*callScope); ok {
			return s
		}
	}
	return &callScope{}
}
