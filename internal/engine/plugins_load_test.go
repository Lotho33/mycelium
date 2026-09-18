package engine

import (
	"os"
	"path/filepath"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// Any plugin present in the repo's plugins/ dir must at least parse and load
// (syntax + require resolution) through the same pool loader the engine uses at
// runtime. This does NOT hit the network or run tasks — it only builds one
// LState per plugin, which executes init.lua's top level (requires + constants
// + function defs). Catches a broken `require`, a syntax slip, or a renamed
// helper file. mycelium ships no plugins of its own, so on a clean checkout
// this test simply has nothing to do.
func TestBundledPluginsLoad(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		t.Skip("no plugins/ dir — mycelium ships no bundled plugins")
	}
	if err != nil {
		t.Fatalf("read plugins dir: %v", err)
	}

	var sharedDirs []string
	if sd := filepath.Join(root, "shared"); dirExists(sd) {
		sharedDirs = []string{sd}
	}

	loaded := 0
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "shared" {
			continue
		}
		dir := filepath.Join(root, e.Name())
		script := filepath.Join(dir, "init.lua")
		if !fileExists(script) {
			continue // not a Lua plugin
		}
		loaded++
		t.Run(e.Name(), func(t *testing.T) {
			// Register the real SDK so a plugin that touches mycelium.* at
			// module-load time can load. No Redis / network — those modules
			// degrade gracefully when unset.
			setup := func(L *lua.LState) {
				RegisterSDK(L, SDKOpts{PluginID: "loadtest:" + e.Name(), PluginDir: dir}, scopeOf(L))
			}
			pool, err := NewLuaPool(1, script, sharedDirs, setup)
			if err != nil {
				t.Fatalf("%s: load failed: %v", e.Name(), err)
			}
			pool.Close()
		})
	}
	if loaded == 0 {
		t.Skip("plugins/ has no Lua plugins to load")
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
