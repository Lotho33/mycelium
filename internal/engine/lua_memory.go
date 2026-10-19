package engine

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Per-plugin memory profile. gopher-lua has no heap accounting, so the
// profile walks every value reachable from an idle state (globals, the
// upvalues of global functions — where a module's `local` caches live — and
// the registry) and estimates its size with the per-object costs below. It
// is an estimate, calibrated against the Go heap in lua_memory_test.go: good
// for finding which plugin and which table holds the memory, not exact bytes.
// Every pooled state has its own copy of what it loaded, so a plugin's total
// is the sum over its states.

// Estimated costs on 64-bit Go, gopher-lua v1.1.
const (
	memTableBase  = 160 // LTable struct + slice/map headers
	memArraySlot  = 16  // LValue interface in the array part
	memHashEntry  = 120 // map bucket share + keys slice + k2i index
	memBoxedNum   = 8   // LNumber boxed in an interface
	memStringHdr  = 16  // LString boxed in an interface
	memFunction   = 80  // LFunction struct
	memUpvalue    = 56  // *Upvalue
	memUserData   = 64  // LUserData struct (the Go value is not counted)
	memProtoBase  = 200 // FunctionProto struct
	memWalkLimit  = 20_000_000
	memCtxCheckEv = 1 << 16
)

// LuaMemItem is one root of a state's memory: a global, a module-level
// local (upvalue) or the registry. Path is human-readable.
type LuaMemItem struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Tables  int    `json:"tables"`
	Entries int    `json:"entries"`
}

// LuaStateMem is the estimate of one pooled state.
type LuaStateMem struct {
	Bytes     int64 `json:"bytes"`
	Tables    int   `json:"tables"`
	Entries   int   `json:"entries"`
	Truncated bool  `json:"truncated,omitempty"` // walk stopped at memWalkLimit
}

// LuaPluginMem is a plugin's profile. Top merges the states' items by path
// (summed: each state holds its own copy).
type LuaPluginMem struct {
	PluginID string        `json:"plugin_id"`
	Name     string        `json:"name"`
	PoolSize int           `json:"pool_size"`
	Bytes    int64         `json:"bytes"`
	States   []LuaStateMem `json:"states"`
	Busy     int           `json:"busy"` // states in use for the whole wait, not measured
	Top      []LuaMemItem  `json:"top"`
	TookMs   int64         `json:"took_ms"`
	// Indexes are the plugin's mycelium.index tables, kept once in Go for
	// all its states; IndexBytes is their estimated total (not in Bytes).
	Indexes    []LuaIndexInfo `json:"indexes"`
	IndexBytes int64          `json:"index_bytes"`
}

type memAcc struct {
	bytes          int64
	tables, entrys int
}

type memWalker struct {
	seen      map[any]struct{}
	nodes     int
	truncated bool
	ctx       context.Context
	stack     []lua.LValue
	protos    map[*lua.FunctionProto]struct{}
}

func newMemWalker(ctx context.Context) *memWalker {
	return &memWalker{seen: map[any]struct{}{}, protos: map[*lua.FunctionProto]struct{}{}, ctx: ctx}
}

func (w *memWalker) mark(p any) bool {
	if _, ok := w.seen[p]; ok {
		return false
	}
	w.seen[p] = struct{}{}
	return true
}

// walk sums everything reachable from v not already counted by an earlier
// walk on this walker (first root wins).
func (w *memWalker) walk(v lua.LValue) memAcc {
	var a memAcc
	w.stack = append(w.stack[:0], v)
	for len(w.stack) > 0 {
		if w.nodes >= memWalkLimit {
			w.truncated = true
			break
		}
		w.nodes++
		if w.nodes%memCtxCheckEv == 0 && w.ctx.Err() != nil {
			w.truncated = true
			break
		}
		cur := w.stack[len(w.stack)-1]
		w.stack = w.stack[:len(w.stack)-1]
		switch x := cur.(type) {
		case lua.LString:
			a.bytes += memStringHdr + int64(len(x))
		case lua.LNumber:
			a.bytes += memBoxedNum
		case *lua.LTable:
			if !w.mark(x) {
				continue
			}
			a.tables++
			a.bytes += memTableBase
			n := x.Len()
			x.ForEach(func(k, val lua.LValue) {
				a.entrys++
				if kn, ok := k.(lua.LNumber); ok && float64(kn) >= 1 && float64(kn) <= float64(n) && float64(kn) == math.Trunc(float64(kn)) {
					a.bytes += memArraySlot
				} else {
					a.bytes += memHashEntry
					if ks, ok := k.(lua.LString); ok {
						a.bytes += int64(len(ks)) // strdict key: no interface box
					} else {
						w.stack = append(w.stack, k)
					}
				}
				w.stack = append(w.stack, val)
			})
			if x.Metatable != nil && x.Metatable != lua.LNil {
				w.stack = append(w.stack, x.Metatable)
			}
		case *lua.LFunction:
			if !w.mark(x) {
				continue
			}
			a.bytes += memFunction + int64(len(x.Upvalues))*memUpvalue
			for _, uv := range x.Upvalues {
				if uv != nil {
					w.stack = append(w.stack, uv.Value())
				}
			}
			if x.Env != nil {
				w.stack = append(w.stack, x.Env)
			}
			if x.Proto != nil {
				a.bytes += w.protoBytes(x.Proto)
			}
		case *lua.LUserData:
			if !w.mark(x) {
				continue
			}
			a.bytes += memUserData
			if x.Env != nil {
				w.stack = append(w.stack, x.Env)
			}
			if x.Metatable != nil {
				w.stack = append(w.stack, x.Metatable)
			}
		}
	}
	w.stack = w.stack[:0]
	return a
}

// protoBytes is the compiled code of a function and its nested ones, counted
// once per state.
func (w *memWalker) protoBytes(p *lua.FunctionProto) int64 {
	if _, ok := w.protos[p]; ok {
		return 0
	}
	w.protos[p] = struct{}{}
	b := int64(memProtoBase + len(p.Code)*4 + len(p.Constants)*16 + len(p.DbgSourcePositions)*8 + len(p.DbgLocals)*48)
	for _, c := range p.Constants {
		if s, ok := c.(lua.LString); ok {
			b += int64(len(s))
		}
	}
	for _, sub := range p.FunctionPrototypes {
		b += w.protoBytes(sub)
	}
	return b
}

// profileState walks one idle state and returns its total and roots.
func profileState(ctx context.Context, L *lua.LState) (LuaStateMem, []LuaMemItem) {
	w := newMemWalker(ctx)
	items := map[string]*LuaMemItem{}
	add := func(path string, a memAcc) {
		if a.bytes == 0 {
			return
		}
		it := items[path]
		if it == nil {
			it = &LuaMemItem{Path: path}
			items[path] = it
		}
		it.Bytes += a.bytes
		it.Tables += a.tables
		it.Entries += a.entrys
	}

	globals := L.G.Global
	// Every function's Env is the globals table: counted explicitly at the
	// end, never through the first local that reaches it.
	w.mark(globals)
	// Module-level locals first: a big cache is usually a `local` captured by
	// the module's global functions, and should be reported under its name.
	var funcs []*lua.LFunction
	var tables []string
	globals.ForEach(func(k, v lua.LValue) {
		switch x := v.(type) {
		case *lua.LFunction:
			funcs = append(funcs, x)
		case *lua.LTable:
			tables = append(tables, k.String())
		}
	})
	// Level by level: data upvalues first, then the upvalues of the
	// functions found in upvalues (a local helper capturing the cache), so
	// the cache is reported under its own name, not the helper's.
	level := funcs
	expanded := map[*lua.LFunction]bool{}
	for depth := 0; depth < 4 && len(level) > 0; depth++ {
		var next []*lua.LFunction
		for _, fn := range level {
			if fn.IsG || fn.Proto == nil || expanded[fn] {
				continue
			}
			expanded[fn] = true
			src := filepath.Base(fn.Proto.SourceName)
			if src == "<string>" {
				src = "init.lua" // the pool runs init.lua with DoString
			}
			for i, uv := range fn.Upvalues {
				if uv == nil {
					continue
				}
				v := uv.Value()
				if f, ok := v.(*lua.LFunction); ok {
					next = append(next, f)
					continue
				}
				name := "?"
				if i < len(fn.Proto.DbgUpvalues) {
					name = fn.Proto.DbgUpvalues[i]
				}
				add(fmt.Sprintf("local %s (%s)", name, src), w.walk(v))
			}
		}
		level = next
	}
	sort.Strings(tables)
	for _, name := range tables {
		add("global "+name, w.walk(globals.RawGetString(name)))
	}
	// What's left: the functions themselves (code), scalar globals, the
	// registry (package.loaded, the SDK's tables).
	var code memAcc
	for _, fn := range funcs {
		a := w.walk(fn)
		code.bytes += a.bytes
		code.tables += a.tables
		code.entrys += a.entrys
	}
	add("funzioni e codice", code)
	rest := memAcc{bytes: memTableBase, tables: 1}
	globals.ForEach(func(k, v lua.LValue) {
		rest.entrys++
		rest.bytes += memHashEntry + int64(len(k.String()))
		a := w.walk(v)
		rest.bytes += a.bytes
		rest.tables += a.tables
		rest.entrys += a.entrys
	})
	add("globali semplici e stdlib", rest)
	add("registry (moduli, SDK)", w.walk(L.Get(lua.RegistryIndex)))

	var st LuaStateMem
	out := make([]LuaMemItem, 0, len(items))
	for _, it := range items {
		st.Bytes += it.Bytes
		st.Tables += it.Tables
		st.Entries += it.Entries
		out = append(out, *it)
	}
	st.Truncated = w.truncated
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return st, out
}

// Profile measures every state of the pool. It borrows them one at a time
// and keeps them until the end, so each state is measured once; a state
// still busy after wait is skipped (busy). Holding p.mu keeps a Reload from
// draining the pool meanwhile.
func (p *LuaPool) Profile(ctx context.Context, wait time.Duration) (states []LuaStateMem, items []LuaMemItem, busy int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var held []*lua.LState
	defer func() {
		for _, L := range held {
			p.pool <- L
		}
	}()
	deadline := time.Now().Add(wait)
	for i := 0; i < p.size; i++ {
		actx, cancel := context.WithDeadline(ctx, deadline)
		L, _, err := p.Acquire(actx)
		cancel()
		if err != nil {
			busy = p.size - i
			break
		}
		held = append(held, L)
	}
	merged := map[string]*LuaMemItem{}
	for _, L := range held {
		st, its := profileState(ctx, L)
		states = append(states, st)
		for _, it := range its {
			m := merged[it.Path]
			if m == nil {
				m = &LuaMemItem{Path: it.Path}
				merged[it.Path] = m
			}
			m.Bytes += it.Bytes
			m.Tables += it.Tables
			m.Entries += it.Entries
		}
	}
	for _, m := range merged {
		items = append(items, *m)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Bytes > items[j].Bytes })
	return states, items, busy
}

// memTopItems is how many roots per plugin the profile reports.
const memTopItems = 12

// MemoryProfile measures every loaded plugin, largest first. It holds each
// plugin's states for the duration of its walk (milliseconds to a few
// hundred for big catalogs), so it runs on demand only.
func (m *LuaPluginManager) MemoryProfile(ctx context.Context) []LuaPluginMem {
	m.mu.RLock()
	plugins := make([]*LuaPlugin, 0, len(m.plugins))
	for _, p := range m.plugins {
		plugins = append(plugins, p)
	}
	m.mu.RUnlock()

	out := make([]LuaPluginMem, 0, len(plugins))
	for _, p := range plugins {
		if p.Pool == nil {
			continue
		}
		start := time.Now()
		states, items, busy := p.Pool.Profile(ctx, 3*time.Second)
		pm := LuaPluginMem{
			PluginID: p.Manifest.ID,
			Name:     p.Manifest.Name,
			PoolSize: p.Pool.size,
			States:   states,
			Busy:     busy,
			TookMs:   time.Since(start).Milliseconds(),
		}
		for _, s := range states {
			pm.Bytes += s.Bytes
		}
		if len(items) > memTopItems {
			items = items[:memTopItems]
		}
		pm.Top = items
		pm.Indexes = pluginIndexes.infos(p.Manifest.ID)
		for _, ix := range pm.Indexes {
			pm.IndexBytes += ix.Bytes
		}
		out = append(out, pm)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Bytes+out[i].IndexBytes > out[j].Bytes+out[j].IndexBytes
	})
	return out
}
