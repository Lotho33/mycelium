package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// LuaPool keeps a fixed pool of pre-loaded *lua.LState for one plugin
// script: Acquire one, use it, release it. Reload rebuilds the pool (hot
// reload).
//
// Every state belongs to a generation; Reload starts a new one. A state of
// an older generation is closed when it comes back instead of re-entering
// the pool, so at most size current states exist and returning one never
// blocks on a full channel.
type LuaPool struct {
	pool chan *lua.LState
	size int
	// genMu guards gen and gens (state -> generation it was created in).
	genMu sync.Mutex
	gen   uint64
	gens  map[*lua.LState]uint64
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
		gens:           make(map[*lua.LState]uint64),
	}
	for i := 0; i < size; i++ {
		L, err := p.newState()
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("lua pool init: %w", err)
		}
		p.track(L, 0)
		p.pool <- L
	}
	return p, nil
}

func (p *LuaPool) track(L *lua.LState, gen uint64) {
	p.genMu.Lock()
	p.gens[L] = gen
	p.genMu.Unlock()
}

// put returns L to the pool if it belongs to the current generation, and
// closes it otherwise.
func (p *LuaPool) put(L *lua.LState) {
	p.genMu.Lock()
	defer p.genMu.Unlock()
	if g, ok := p.gens[L]; ok && g == p.gen {
		select {
		case p.pool <- L:
			return
		default: // cannot happen while the invariant holds; never block
		}
	}
	delete(p.gens, L)
	L.Close()
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
		return L, func() { p.put(L) }, nil
	case <-ctx.Done():
		return nil, func() {}, ctx.Err()
	}
}

// DiscardAndReplace is used instead of release() when old may still be in
// use by an abandoned goroutine (a timed-out call): old is dropped for good
// (not closed: its goroutine still runs on it) and, if it belonged to the
// current generation, a fresh state takes its place, keeping the pool size.
func (p *LuaPool) DiscardAndReplace(old *lua.LState) {
	p.genMu.Lock()
	g, ok := p.gens[old]
	delete(p.gens, old)
	current := ok && g == p.gen
	p.genMu.Unlock()
	if !current {
		return // a Reload already replaced it
	}
	L, err := p.newState()
	if err != nil {
		// The pool shrinks by one state until the next Reload.
		log.Printf("[lua] pool discard-replace: could not create replacement state: %v", err)
		return
	}
	p.track(L, g)
	p.put(L)
}

// Reload re-reads the script and replaces every pooled state.
//
// The new states are built first: if the script no longer loads, the pool
// keeps serving the old code and the error is returned. Borrowed states of
// the old generation are closed when released (see put).
func (p *LuaPool) Reload() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	fresh := make([]*lua.LState, 0, p.size)
	for i := 0; i < p.size; i++ {
		L, err := p.newState()
		if err != nil {
			for _, f := range fresh {
				f.Close()
			}
			return fmt.Errorf("lua pool reload: %w", err)
		}
		fresh = append(fresh, L)
	}

	p.genMu.Lock()
	p.gen++
	for {
		select {
		case L := <-p.pool:
			delete(p.gens, L)
			L.Close()
			continue
		default:
		}
		break
	}
	for _, L := range fresh {
		p.gens[L] = p.gen
		p.pool <- L
	}
	p.genMu.Unlock()
	return nil
}

// Close shuts down every pooled LState. After Close the pool must not be used.
func (p *LuaPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.genMu.Lock()
	defer p.genMu.Unlock()
	p.gen++ // borrowed states are closed when released
	for {
		select {
		case L := <-p.pool:
			delete(p.gens, L)
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

	// coroutine too: a coroutine created outside a call keeps no context, so
	// a loop in it would outlive the call's timeout. No plugin uses it.
	for _, g := range []string{"io", "debug", "dofile", "loadfile", "load", "loadstring", "coroutine"} {
		L.SetGlobal(g, lua.LNil)
	}
	// string.rep is the one stdlib call that allocates without bound from a
	// single line ("x"):rep(2^34); the string metatable indexes this same
	// table, so the method form is capped too.
	if str, ok := L.GetGlobal("string").(*lua.LTable); ok {
		L.SetField(str, "rep", L.NewFunction(cappedStringRep))
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

// maxStringRepBytes caps the result of string.rep.
const maxStringRepBytes = 32 << 20

func cappedStringRep(L *lua.LState) int {
	str := L.CheckString(1)
	n := L.CheckInt(2)
	sep := L.OptString(3, "")
	if n <= 0 {
		L.Push(lua.LString(""))
		return 1
	}
	if int64(len(str)+len(sep))*int64(n) > maxStringRepBytes {
		L.RaiseError("string.rep: result over %d bytes", maxStringRepBytes)
	}
	var b strings.Builder
	b.Grow((len(str) + len(sep)) * n)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(str)
	}
	L.Push(lua.LString(b.String()))
	return 1
}

// initScriptTimeout bounds init.lua's top level (and the eager module
// loads): it only defines functions and tables.
const initScriptTimeout = 30 * time.Second

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
	// preloadDir registers the .lua files in dir as require()-able modules
	// and returns their names.
	preloadDir := func(dir string, skipInit bool) (names []string) {
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
			names = append(names, modName)
		}
		return names
	}
	// Shared modules first; plugin modules with the same name override them.
	for _, dir := range p.sharedDirs {
		preloadDir(dir, false)
	}
	// Plugin-specific modules.
	modules := preloadDir(filepath.Dir(p.script), true)
	src, err := os.ReadFile(p.script)
	if err != nil {
		L.Close()
		return nil, fmt.Errorf("read script %s: %w", p.script, err)
	}
	// The top level runs outside any call: bounded by a deadline, and with
	// the network closed (requireProxy without a proxy fails every request),
	// so it can't hang the load or go out around the plugin's egress.
	ctx, cancel := context.WithTimeout(context.Background(), initScriptTimeout)
	defer cancel()
	L.SetContext(ctx)
	sc := scopeOf(L)
	sc.set(ctx, "", true, "", nil)
	if err := L.DoString(string(src)); err != nil {
		L.Close()
		return nil, fmt.Errorf("exec script %s: %w", p.script, err)
	}
	// Load the plugin's modules now, so the globals they define are part of
	// the baseline below (loaded lazily inside a call they'd be wiped after
	// it). Best effort: a module that fails here still loads on demand.
	require := L.GetGlobal("require")
	for _, name := range modules {
		if err := L.CallByParam(lua.P{Fn: require, NRet: 0, Protect: true}, lua.LString(name)); err != nil {
			log.Printf("[lua] %s: modulo %s non precaricato: %v", filepath.Base(filepath.Dir(p.script)), name, err)
		}
	}
	sc.reset()
	L.RemoveContext()
	// Snapshot _G right after the top-level script: later calls wipe any global
	// they add (callScope.resetNewGlobals), so nothing leaks to the next call on
	// this state — possibly another profile's.
	scopeOf(L).snapshotGlobalBaseline(L)
	return L, nil
}
