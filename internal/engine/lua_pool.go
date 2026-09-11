package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	lua "github.com/yuin/gopher-lua"
)

// LuaPool manages a fixed pool of pre-loaded *lua.LState instances for a single
// plugin script. Callers Acquire() an LState, use it, then release it via the
// returned function. Reload() tears down all states and rebuilds the pool,
// implementing hot-reload on script change.
type LuaPool struct {
	pool       chan *lua.LState
	size       int
	script     string            // absolute path to the .lua file
	sharedDirs []string          // directories whose .lua files are preloaded as shared modules
	setupFn    func(*lua.LState) // called on every new LState (registers SDK modules)
	mu         sync.RWMutex
}

// NewLuaPool creates a pool of `size` LState instances, each pre-loaded with
// the given script. sharedDirs lists directories whose .lua files are available
// via require() in every plugin (loaded before plugin-specific modules).
// setupFn (if non-nil) is called on every fresh LState before the script is executed.
func NewLuaPool(size int, scriptPath string, sharedDirs []string, setupFn func(*lua.LState)) (*LuaPool, error) {
	if size <= 0 {
		size = 4
	}
	p := &LuaPool{
		pool:       make(chan *lua.LState, size),
		size:       size,
		script:     scriptPath,
		sharedDirs: sharedDirs,
		setupFn:    setupFn,
	}
	for i := 0; i < size; i++ {
		L, err := p.newState()
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("lua pool init: %w", err)
		}
		p.pool <- L
	}
	return p, nil
}

// Acquire borrows an LState from the pool. Returns an error if ctx is cancelled
// before a state becomes available. The caller MUST call release() when done.
func (p *LuaPool) Acquire(ctx context.Context) (*lua.LState, func(), error) {
	select {
	case L := <-p.pool:
		return L, func() { p.pool <- L }, nil
	case <-ctx.Done():
		return nil, func() {}, ctx.Err()
	}
}

// DiscardAndReplace should be called INSTEAD of the normal release() when the
// LState might still be in active use by a goroutine that hasn't returned —
// e.g. after giving up on a stuck Lua call via timeout. Pushing that same
// LState back into the pool would let a future Acquire() hand it to another
// caller while the abandoned goroutine is still mutating it concurrently
// (gopher-lua's LState is not safe for concurrent use), corrupting it in a
// way that could then surface as an unrelated failure much later. Instead
// this creates a fresh replacement state and adds THAT to the pool, keeping
// pool size stable. The old, possibly-still-running LState is deliberately
// leaked — Go has no way to force-kill a goroutine — but it stays isolated,
// never handed to anyone else again.
func (p *LuaPool) DiscardAndReplace() {
	L, err := p.newState()
	if err != nil {
		// Pool permanently shrinks by one slot — logged by the caller via
		// the returned error path isn't available here (no caller signal),
		// so this is a best-effort log; not fatal, just reduced concurrency
		// for this plugin until the next Reload().
		log.Printf("[lua] pool discard-replace: could not create replacement state: %v", err)
		return
	}
	p.pool <- L
}

// Reload re-reads the script from disk and replaces all pooled LStates.
// Safe to call concurrently — drains the pool under write lock.
func (p *LuaPool) Reload() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Drain existing states.
	n := len(p.pool)
	for i := 0; i < n; i++ {
		L := <-p.pool
		L.Close()
	}
	// Refill.
	for i := 0; i < p.size; i++ {
		L, err := p.newState()
		if err != nil {
			return fmt.Errorf("lua pool reload: %w", err)
		}
		p.pool <- L
	}
	return nil
}

// Close shuts down every pooled LState. After Close the pool must not be used.
func (p *LuaPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		select {
		case L := <-p.pool:
			L.Close()
		default:
			return
		}
	}
}

// lockdownStdlib strips the parts of the Lua standard library a plugin has no
// legitimate need for and that turn "a buggy or malicious plugin" into
// "arbitrary code / file / process control as the mycelium user": the process
// controls in os (execute/exit/remove/rename/getenv/…), the whole io and debug
// tables, dynamic code loading (load/loadfile/dofile), and native-library
// loading (package.loadlib). This is damage limitation for a mono-tenant box
// where the admin installs the plugins — NOT a boundary against a determined
// attacker who already runs code in this process. Plugins keep base, table,
// string, math, coroutine, require(), and os.time/date/clock/difftime (used
// widely for scheduling/formatting).
func lockdownStdlib(L *lua.LState) {
	safeOS := L.NewTable()
	if osv, ok := L.GetGlobal("os").(*lua.LTable); ok {
		for _, fn := range []string{"time", "date", "clock", "difftime"} {
			L.SetField(safeOS, fn, osv.RawGetString(fn))
		}
	}
	L.SetGlobal("os", safeOS)

	for _, g := range []string{"io", "debug", "dofile", "loadfile", "load", "loadstring"} {
		L.SetGlobal(g, lua.LNil)
	}
	if pkg, ok := L.GetGlobal("package").(*lua.LTable); ok {
		L.SetField(pkg, "loadlib", lua.LNil)
		L.SetField(pkg, "cpath", lua.LString(""))
	}
}

// newState creates, configures, and pre-loads a single LState.
func (p *LuaPool) newState() (*lua.LState, error) {
	L := lua.NewState()
	lockdownStdlib(L)
	// Every pooled state carries its own callScope, reused for every call it
	// serves. setupFn (RegisterSDK) reads it back via scopeOf(L).
	attachScope(L, &callScope{})
	if p.setupFn != nil {
		p.setupFn(L)
	}
	// preloadDir registers all .lua files in dir as require()-able modules.
	// Plugin-specific files (loaded after) override shared ones with the same name.
	preloadDir := func(dir string, skipInit bool) {
		files, _ := filepath.Glob(filepath.Join(dir, "*.lua"))
		for _, f := range files {
			if skipInit && filepath.Base(f) == "init.lua" {
				continue
			}
			fPath := f
			modName := filepath.Base(f)
			modName = modName[:len(modName)-4] // strip ".lua"
			L.PreloadModule(modName, func(L *lua.LState) int {
				if err := L.DoFile(fPath); err != nil {
					L.RaiseError("preload %s: %v", modName, err)
				}
				L.Push(lua.LTrue)
				return 1
			})
		}
	}
	// Shared modules first (plugin-specific overrides if same name exists).
	for _, dir := range p.sharedDirs {
		preloadDir(dir, false)
	}
	// Plugin-specific modules.
	preloadDir(filepath.Dir(p.script), true)
	src, err := os.ReadFile(p.script)
	if err != nil {
		L.Close()
		return nil, fmt.Errorf("read script %s: %w", p.script, err)
	}
	if err := L.DoString(string(src)); err != nil {
		L.Close()
		return nil, fmt.Errorf("exec script %s: %w", p.script, err)
	}
	return L, nil
}
