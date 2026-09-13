package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// A pooled LState must not hand plugins process/file control, but must keep the
// harmless bits they actually use.
func TestLuaPool_StdlibLockdown(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "init.lua")
	if err := os.WriteFile(script, []byte(`
		results = {
			os_execute   = tostring(os.execute),
			os_exit      = tostring(os.exit),
			os_remove    = tostring(os.remove),
			os_getenv    = tostring(os.getenv),
			os_time_type = type(os.time),
			io_tab       = tostring(io),
			debug_tab    = tostring(debug),
			loadfile     = tostring(loadfile),
			dofile       = tostring(dofile),
			load_fn      = tostring(load),
			require_type = type(require),
			pkg_loadlib  = tostring(package and package.loadlib),
		}
	`), 0o644); err != nil {
		t.Fatal(err)
	}

	pool, err := NewLuaPool(1, script, nil, nil)
	if err != nil {
		t.Fatalf("pool init: %v", err)
	}
	defer pool.Close()

	L, release, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	got := map[string]string{}
	tbl := L.GetGlobal("results").(*lua.LTable)
	tbl.ForEach(func(k, v lua.LValue) { got[k.String()] = v.String() })

	nilFields := []string{
		"os_execute", "os_exit", "os_remove", "os_getenv",
		"io_tab", "debug_tab", "loadfile", "dofile", "load_fn", "pkg_loadlib",
	}
	for _, f := range nilFields {
		if got[f] != "nil" {
			t.Errorf("%s = %q, want nil", f, got[f])
		}
	}
	if got["os_time_type"] != "function" {
		t.Errorf("os.time type = %q, want function", got["os_time_type"])
	}
	if got["require_type"] != "function" {
		t.Errorf("require type = %q, want function", got["require_type"])
	}
}

// require() in gopher-lua resolves through package.preload first, then falls
// back to a filesystem search driven by package.path (os.Stat + LoadFile at
// the Go level — outside any of the globals lockdownStdlib neutralizes).
// Left untouched, package.path defaults to a cwd-relative pattern, so any
// plugin could require("plugins.<other_plugin_id>.init") and load another
// installed plugin's source into its own LState. This simulates that attack:
// it chdirs into a directory laid out like the production WORKDIR, with a
// "victim" plugin sitting where the default package.path would find it, and
// asserts an attacker plugin's require() of it fails and never executes the
// victim's code.
func TestLuaPool_RequireCannotEscapeToFilesystem(t *testing.T) {
	root := t.TempDir()
	victimDir := filepath.Join(root, "plugins", "victim")
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victimDir, "init.lua"), []byte(`
		victim_loaded = true
		return { secret = "victim's logic" }
	`), 0o644); err != nil {
		t.Fatal(err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(origWD); err != nil {
			t.Fatal(err)
		}
	}()

	attackerDir := filepath.Join(root, "attacker")
	if err := os.MkdirAll(attackerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(attackerDir, "init.lua")
	if err := os.WriteFile(script, []byte(`
		local ok, err = pcall(require, "plugins.victim.init")
		results = {
			ok = tostring(ok),
			victim_loaded = tostring(victim_loaded),
		}
	`), 0o644); err != nil {
		t.Fatal(err)
	}

	pool, err := NewLuaPool(1, script, nil, nil)
	if err != nil {
		t.Fatalf("pool init: %v", err)
	}
	defer pool.Close()

	L, release, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	tbl, ok := L.GetGlobal("results").(*lua.LTable)
	if !ok {
		t.Fatal("results global not set")
	}
	got := map[string]string{}
	tbl.ForEach(func(k, v lua.LValue) { got[k.String()] = v.String() })

	if got["ok"] != "false" {
		t.Errorf("require(\"plugins.victim.init\") ok = %q, want false (must fail to resolve on the filesystem)", got["ok"])
	}
	if got["victim_loaded"] != "nil" {
		t.Errorf("victim_loaded = %q, want nil — victim plugin source must never execute", got["victim_loaded"])
	}
}

// Blanking package.path must not break the one require() pattern this
// codebase actually relies on: a module explicitly registered via
// PreloadModule (how preloadDir in newState exposes plugins/shared/*.lua and
// a plugin's own sibling files). package.preload is checked by gopher-lua's
// loader chain before package.path is ever consulted, so this must keep
// working after the fix.
func TestLuaPool_RequirePreloadedSharedModuleStillWorks(t *testing.T) {
	// preloadDir's loader (lua_pool.go) runs the shared file with DoFile and
	// always pushes `true` as the module's value — it does not forward a
	// Lua-style `return {...}` from the file. So the real convention here
	// (per docs/plugin-sdk.md: "require its helpers") is that a shared module
	// defines globals as a side effect of require()'ing it once, not that it
	// hands back a table; mirror that instead of standard Lua module shape.
	sharedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sharedDir, "vix_common.lua"), []byte(`
		function vix_greet() return "hello from vix_common" end
	`), 0o644); err != nil {
		t.Fatal(err)
	}

	pluginDir := t.TempDir()
	script := filepath.Join(pluginDir, "init.lua")
	if err := os.WriteFile(script, []byte(`
		require("vix_common")
		results = { greeting = vix_greet() }
	`), 0o644); err != nil {
		t.Fatal(err)
	}

	pool, err := NewLuaPool(1, script, []string{sharedDir}, nil)
	if err != nil {
		t.Fatalf("pool init: %v", err)
	}
	defer pool.Close()

	L, release, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	tbl, ok := L.GetGlobal("results").(*lua.LTable)
	if !ok {
		t.Fatal("results global not set")
	}
	if got := tbl.RawGetString("greeting").String(); got != "hello from vix_common" {
		t.Errorf("require(\"vix_common\").greet() = %q, want %q", got, "hello from vix_common")
	}
}

// A manifest's pool_size is attacker/uploader-influenced (an admin ZIP upload
// need not be operator-authored), and NewLuaPool builds every slot
// synchronously at load time — an absurd value must be clamped to
// maxPoolSize, not honoured, or loading the plugin alone can stall boot or
// exhaust memory.
func TestLoadPlugin_PoolSizeClamped(t *testing.T) {
	const initLua = "function probe() return {} end"

	cases := []struct {
		name     string
		manifest string
		want     int
	}{
		{
			name:     "absurd pool_size clamped to max",
			manifest: "id: t.huge\nname: T\ndirect_egress: true\npool_size: 100000\nentrypoints:\n  probe: probe\n",
			want:     maxPoolSize,
		},
		{
			name:     "in-range pool_size honoured as-is",
			manifest: "id: t.five\nname: T\ndirect_egress: true\npool_size: 5\nentrypoints:\n  probe: probe\n",
			want:     5,
		},
		{
			name:     "omitted pool_size keeps the default of 2",
			manifest: "id: t.default\nname: T\ndirect_egress: true\nentrypoints:\n  probe: probe\n",
			want:     2,
		},
		{
			name:     "non-positive pool_size keeps the default of 2",
			manifest: "id: t.zero\nname: T\ndirect_egress: true\npool_size: 0\nentrypoints:\n  probe: probe\n",
			want:     2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTestPlugin(t, tc.manifest, initLua)
			m := newTestManager(t)
			if err := m.loadPlugin(dir); err != nil {
				t.Fatalf("loadPlugin: %v", err)
			}
			// Each subtest uses its own fresh manager with exactly one loaded
			// plugin, so there is no ambiguity in picking it out by id.
			m.mu.RLock()
			var (
				got   int
				found bool
			)
			for _, pl := range m.plugins {
				got = pl.Pool.size
				found = true
			}
			m.mu.RUnlock()
			if !found {
				t.Fatalf("plugin not found in manager after loadPlugin")
			}
			if got != tc.want {
				t.Errorf("pool size = %d, want %d", got, tc.want)
			}
		})
	}
}
