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

// LuaPool keeps a fixed pool of pre-loaded *lua.LState for one plugin
// script: Acquire one, use it, release it. Reload rebuilds the pool (hot
// reload).
type LuaPool struct {
	pool chan *lua.LState
	size int
	// nonInteractive is the single budget (size-1 states) shared by background
	// tasks and catalog fetches; see BackgroundSlot.
	nonInteractive chan struct{}
	script         string            // absolute path to the .lua file
	sharedDirs     []string          // directories whose .lua files are preloaded as shared modules
	setupFn        func(*lua.LState) // called on every new LState (registers SDK modules)
	mu             sync.RWMutex
}

// NewLuaPool creates a pool of size states, each running script. sharedDirs
// holds modules require()-able by every plugin (plugin modules override
// them); setupFn runs on every fresh state before the script.
func NewLuaPool(size int, scriptPath string, sharedDirs []string, setupFn func(*lua.LState)) (*LuaPool, error) {
	if size <= 0 {
		size = 4
	}
	p := &LuaPool{
		pool:           make(chan *lua.LState, size),
		size:           size,
		nonInteractive: make(chan struct{}, backgroundSlots(size)),
		script:         scriptPath,
		sharedDirs:     sharedDirs,
		setupFn:        setupFn,
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

// backgroundSlots is how many states non-interactive work may hold at once:
// all but one, so a user-facing call always finds a free state. A
// single-state pool can't reserve anything.
func backgroundSlots(size int) int {
	if size <= 1 {
		return 1
	}
	return size - 1
}

// BackgroundSlot reserves a slot for non-interactive work (blocking until
// one frees up or ctx ends). Cron tasks and the catalog-count warmup use it;
// CatalogSlot draws from the same budget. It must be one shared budget:
// two independent ones could be held at the same time and leave no state
// for an interactive caller.
//
// Take it before Acquire and give it back with the returned func once done,
// also after a timeout that discarded the state.
func (p *LuaPool) BackgroundSlot(ctx context.Context) (func(), error) {
	return takeSlot(ctx, p.nonInteractive)
}

// CatalogSlot is BackgroundSlot for GetCatalog calls (same budget).
func (p *LuaPool) CatalogSlot(ctx context.Context) (func(), error) {
	return takeSlot(ctx, p.nonInteractive)
}

func takeSlot(ctx context.Context, slots chan struct{}) (func(), error) {
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Stats reports idle states and pool size, for logging (a racy snapshot).
func (p *LuaPool) Stats() (free, size int) {
	return len(p.pool), p.size
}

// Acquire borrows an LState. It fails if ctx ends first. The caller must
// call release() when done.
func (p *LuaPool) Acquire(ctx context.Context) (*lua.LState, func(), error) {
	select {
	case L := <-p.pool:
		return L, func() { p.pool <- L }, nil
	case <-ctx.Done():
		return nil, func() {}, ctx.Err()
	}
}

// DiscardAndReplace is used instead of release() when the state may still
// be in use by an abandoned goroutine (a timed-out call): the state is
// dropped for good and a fresh one takes its place, keeping the pool size.
func (p *LuaPool) DiscardAndReplace() {
	L, err := p.newState()
	if err != nil {
		// The pool shrinks by one state until the next Reload.
		log.Printf("[lua] pool discard-replace: could not create replacement state: %v", err)
		return
	}
	p.pool <- L
}

// Reload re-reads the script and replaces every pooled state.
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

// lockdownStdlib removes what a plugin doesn't need and would turn a buggy
// or malicious plugin into process control: os beyond time/date/clock/
// difftime, io, debug, load/loadfile/dofile/loadstring and package.loadlib.
// Damage limitation for admin-installed plugins, not a hard boundary.
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
		// package.path drives gopher-lua's filesystem require() loader; left at its
		// default a plugin could load another plugin's source. Legitimate modules
		// resolve through package.preload (preloadDir below), so blanking it breaks
		// nothing.
		L.SetField(pkg, "path", lua.LString(""))
	}
	// The package table stays writable, so also drop the filesystem loader
	// itself from the registry's _LOADERS, keeping only the preload loader.
	if loaders, ok := L.GetField(L.Get(lua.RegistryIndex), "_LOADERS").(*lua.LTable); ok {
		for i := loaders.Len(); i > 1; i-- {
			loaders.RawSetInt(i, lua.LNil)
		}
	}
}

// newState creates, configures, and pre-loads a single LState.
func (p *LuaPool) newState() (*lua.LState, error) {
	L := lua.NewState()
	lockdownStdlib(L)
	// Every pooled state has its own callScope, reused for every call it serves
	// (read by RegisterSDK via scopeOf(L)).
	attachScope(L, &callScope{})
	if p.setupFn != nil {
		p.setupFn(L)
	}
	// preloadDir registers the .lua files in dir as require()-able modules.
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
	// Shared modules first; plugin modules with the same name override them.
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
	// Snapshot _G right after the top-level script: later calls wipe any global
	// they add (callScope.resetNewGlobals), so nothing leaks to the next call on
	// this state — possibly another profile's.
	scopeOf(L).snapshotGlobalBaseline(L)
	return L, nil
}
