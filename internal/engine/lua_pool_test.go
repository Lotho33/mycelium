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
