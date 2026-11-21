package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// docs/mycelium.meta.lua (editor annotations for plugin authors) must list
// exactly what RegisterSDK exposes: a function added to the SDK without an
// annotation, or an annotation left for one removed, fails here.
func TestSDKMetaMatchesRegisteredSDK(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	attachScope(L, &callScope{})
	RegisterSDK(L, SDKOpts{PluginID: "meta"}, scopeOf(L))
	registered := map[string]bool{}
	var walk func(prefix string, tb *lua.LTable)
	walk = func(prefix string, tb *lua.LTable) {
		tb.ForEach(func(k, v lua.LValue) {
			name := prefix + "." + k.String()
			if sub, ok := v.(*lua.LTable); ok {
				walk(name, sub)
				return
			}
			registered[name] = true
		})
	}
	walk("mycelium", L.GetGlobal("mycelium").(*lua.LTable))

	src, err := os.ReadFile(filepath.Join("..", "..", "docs", "mycelium.meta.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := L.LoadString(string(src)); err != nil {
		t.Fatalf("docs/mycelium.meta.lua is not valid Lua: %v", err)
	}
	annotated := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^function (mycelium(?:\.[a-z0-9_]+)+)\(`).FindAllStringSubmatch(string(src), -1) {
		annotated[m[1]] = true
	}
	// Non-function fields, annotated with ---@field on their module's class.
	for _, m := range regexp.MustCompile(`(?m)^---@class (mycelium\.[a-z_]+)\n((?:---@field [^\n]+\n)+)`).FindAllStringSubmatch(string(src), -1) {
		for _, f := range regexp.MustCompile(`---@field ([a-z_]+)`).FindAllStringSubmatch(m[2], -1) {
			annotated[m[1]+"."+f[1]] = true
		}
	}

	var missing, stale []string
	for n := range registered {
		if !annotated[n] {
			missing = append(missing, n)
		}
	}
	for n := range annotated {
		if !registered[n] {
			stale = append(stale, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("SDK without annotation in docs/mycelium.meta.lua:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("annotated but not in the SDK:\n  %s", strings.Join(stale, "\n  "))
	}
	if len(registered) < 40 {
		t.Fatalf("only %d SDK entries found: the walk is broken", len(registered))
	}
}
