package engine

import lua "github.com/yuin/gopher-lua"

// Limits for converting plugin-returned Lua tables to Go/JSON: a
// self-referencing table would recurse until the stack overflows (fatal),
// and shared subtables can expand exponentially. Real payloads stay far
// below both.
const (
	maxLuaConvDepth = 200
	maxLuaConvNodes = 2_000_000
)

// luaConvGuard tracks the tables on the current conversion path (cycles) and
// the total number of tables visited (DAG blow-up). A table it refuses is
// converted as nil/null instead.
type luaConvGuard struct {
	onPath map[*lua.LTable]bool
	nodes  int
}

func newLuaConvGuard() *luaConvGuard {
	return &luaConvGuard{onPath: make(map[*lua.LTable]bool)}
}

// enter reports whether t may be converted; on true the caller must call
// leave(t) once done with it.
func (g *luaConvGuard) enter(t *lua.LTable) bool {
	if g.onPath[t] || len(g.onPath) >= maxLuaConvDepth || g.nodes >= maxLuaConvNodes {
		return false
	}
	g.nodes++
	g.onPath[t] = true
	return true
}

func (g *luaConvGuard) leave(t *lua.LTable) { delete(g.onPath, t) }
