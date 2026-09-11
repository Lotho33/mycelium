package engine

import lua "github.com/yuin/gopher-lua"

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
}

func (s *callScope) set(profileID string, requireProxy bool, proxyURL string, onProgress ProgressFunc) {
	s.profileID = profileID
	s.requireProxy = requireProxy
	s.proxyURL = proxyURL
	s.onProgress = onProgress
}

func (s *callScope) reset() {
	s.profileID = ""
	s.requireProxy = false
	s.proxyURL = ""
	s.onProgress = nil
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
