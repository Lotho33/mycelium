package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// mycelium.index: read-only key → values lookup tables kept in Go and shared
// by every LState of a plugin. A large catalog loaded into Lua tables costs
// several times its JSON size and is copied once per pooled state; here each
// record is stored once, as compact JSON, and decoded only when looked up.
//
// A plugin builds an index with a builder (add records under one or more
// keys, then publish under a name); publishing replaces the previous index
// atomically, so every state sees the new one on its next lookup. Indexes
// live in memory only: the plugin rebuilds them from its own data (storage,
// cache) after a restart or a code reload.

const (
	maxIndexesPerPlugin = 16
	maxIndexBytes       = 256 << 20 // per index, estimated
	maxIndexRecords     = 4_000_000
	maxIndexKeysResult  = 1_000_000 // mycelium.index.keys
)

// luaIndex is one published index. Immutable once published.
type luaIndex struct {
	vals    [][]byte           // JSON of each record
	keys    map[string][]int32 // key → record positions, in insertion order
	bytes   int64              // estimated Go memory
	builtAt time.Time
}

// LuaIndexInfo describes a published index (memory profile, dashboard).
type LuaIndexInfo struct {
	Name    string `json:"name"`
	Records int    `json:"records"`
	Keys    int    `json:"keys"`
	Bytes   int64  `json:"bytes"`
	BuiltAt int64  `json:"built_at"`
}

type indexStore struct {
	mu sync.RWMutex
	m  map[string]map[string]*luaIndex // plugin id → name → index
}

var pluginIndexes = &indexStore{m: map[string]map[string]*luaIndex{}}

func (s *indexStore) get(plugin, name string) *luaIndex {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[plugin][name]
}

func (s *indexStore) put(plugin, name string, ix *luaIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	byName := s.m[plugin]
	if byName == nil {
		byName = map[string]*luaIndex{}
		s.m[plugin] = byName
	}
	if _, ok := byName[name]; !ok && len(byName) >= maxIndexesPerPlugin {
		return fmt.Errorf("index: too many indexes (max %d)", maxIndexesPerPlugin)
	}
	byName[name] = ix
	return nil
}

func (s *indexStore) drop(plugin, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m[plugin], name)
}

// dropPlugin forgets every index of a plugin: on unload, and on a code reload
// (the new code may index differently; it rebuilds on first use).
func (s *indexStore) dropPlugin(plugin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, plugin)
}

// infos lists a plugin's indexes, largest first.
func (s *indexStore) infos(plugin string) []LuaIndexInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]LuaIndexInfo, 0, len(s.m[plugin]))
	for name, ix := range s.m[plugin] {
		out = append(out, ix.info(name))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

func (ix *luaIndex) info(name string) LuaIndexInfo {
	return LuaIndexInfo{Name: name, Records: len(ix.vals), Keys: len(ix.keys), Bytes: ix.bytes, BuiltAt: ix.builtAt.Unix()}
}

// Estimated Go costs, the same spirit as lua_memory.go.
const (
	ixValCost = 24           // slice header in vals
	ixKeyCost = 16 + 24 + 48 // string header + positions slice header + map entry
	ixPosCost = 4
)

// indexBuilder accumulates records until publish. Identical values are
// stored once (id-mapping indexes repeat the same small record a lot).
type indexBuilder struct {
	vals   [][]byte
	keys   map[string][]int32
	dedupe map[string]int32
	bytes  int64
	done   bool
}

const luaIndexBuilderType = "mycelium.index.builder"

func checkIndexBuilder(L *lua.LState) *indexBuilder {
	ud := L.CheckUserData(1)
	b, ok := ud.Value.(*indexBuilder)
	if !ok {
		L.ArgError(1, "index builder expected")
	}
	return b
}

func (b *indexBuilder) add(val []byte, keys []string) error {
	if b.done {
		return fmt.Errorf("index builder already published")
	}
	pos, ok := b.dedupe[string(val)]
	if !ok {
		if len(b.vals) >= maxIndexRecords {
			return fmt.Errorf("index: too many records (max %d)", maxIndexRecords)
		}
		pos = int32(len(b.vals))
		b.vals = append(b.vals, val)
		b.dedupe[string(val)] = pos
		b.bytes += int64(len(val)) + ixValCost
	}
	for _, k := range keys {
		cur, seen := b.keys[k]
		if !seen {
			b.bytes += int64(len(k)) + ixKeyCost
		} else if cur[len(cur)-1] == pos {
			continue // same record twice under one key
		}
		b.keys[k] = append(cur, pos)
		b.bytes += ixPosCost
	}
	if b.bytes > maxIndexBytes {
		return fmt.Errorf("index: too large (max %d MiB)", maxIndexBytes>>20)
	}
	return nil
}

// indexKey is a lookup key from a Lua string or number (numbers as tostring
// prints them, so 123 and "123" are the same key).
func indexKey(v lua.LValue) (string, bool) {
	switch k := v.(type) {
	case lua.LString:
		return string(k), k != ""
	case lua.LNumber:
		return k.String(), true
	}
	return "", false
}

// decodeIndexValue turns a stored record back into a fresh Lua value.
func decodeIndexValue(L *lua.LState, raw []byte) lua.LValue {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return lua.LNil
	}
	return goToLua(L, v)
}

func buildIndexModule(L *lua.LState, opts SDKOpts) *lua.LTable {
	t := L.NewTable()
	plugin := opts.PluginID

	mt := L.NewTypeMetatable(luaIndexBuilderType)
	methods := L.NewTable()
	mt.RawSetString("__index", methods)

	// b:add(value, key1, key2, ...) — stores value (table, string, number,
	// bool) under every non-empty key. Lookups return the values of a key in
	// the order they were added.
	methods.RawSetString("add", L.NewFunction(func(L *lua.LState) int {
		b := checkIndexBuilder(L)
		val, err := json.Marshal(luaToJSON(L.CheckAny(2)))
		if err != nil {
			L.RaiseError("index add: %v", err)
		}
		var keys []string
		for i := 3; i <= L.GetTop(); i++ {
			if k, ok := indexKey(L.Get(i)); ok {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			return 0
		}
		if err := b.add(val, keys); err != nil {
			L.RaiseError("%v", err)
		}
		return 0
	}))

	// b:count() → records added so far (after de-duplication).
	methods.RawSetString("count", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(len(checkIndexBuilder(L).vals)))
		return 1
	}))

	// b:publish(name) → records, err — replaces the index name for every
	// state of the plugin. The builder can't be used afterwards.
	methods.RawSetString("publish", L.NewFunction(func(L *lua.LState) int {
		b := checkIndexBuilder(L)
		name := L.CheckString(2)
		if b.done {
			L.Push(lua.LNil)
			L.Push(lua.LString("index builder already published"))
			return 2
		}
		b.done = true
		ix := &luaIndex{vals: b.vals, keys: b.keys, bytes: b.bytes, builtAt: time.Now()}
		b.vals, b.keys, b.dedupe = nil, nil, nil
		if err := pluginIndexes.put(plugin, name, ix); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LNumber(len(ix.vals)))
		L.Push(lua.LNil)
		return 2
	}))

	// mycelium.index.builder() → builder
	t.RawSetString("builder", L.NewFunction(func(L *lua.LState) int {
		ud := L.NewUserData()
		ud.Value = &indexBuilder{keys: map[string][]int32{}, dedupe: map[string]int32{}}
		L.SetMetatable(ud, L.GetTypeMetatable(luaIndexBuilderType))
		L.Push(ud)
		return 1
	}))

	// mycelium.index.has(name) → bool
	t.RawSetString("has", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LBool(pluginIndexes.get(plugin, L.CheckString(1)) != nil))
		return 1
	}))

	// mycelium.index.get(name, key) → first value under key, or nil
	t.RawSetString("get", L.NewFunction(func(L *lua.LState) int {
		ix := pluginIndexes.get(plugin, L.CheckString(1))
		k, ok := indexKey(L.Get(2))
		if ix == nil || !ok {
			L.Push(lua.LNil)
			return 1
		}
		pos := ix.keys[k]
		if len(pos) == 0 {
			L.Push(lua.LNil)
			return 1
		}
		L.Push(decodeIndexValue(L, ix.vals[pos[0]]))
		return 1
	}))

	// mycelium.index.get_all(name, key) → array of every value under key
	t.RawSetString("get_all", L.NewFunction(func(L *lua.LState) int {
		out := L.NewTable()
		ix := pluginIndexes.get(plugin, L.CheckString(1))
		if k, ok := indexKey(L.Get(2)); ix != nil && ok {
			for _, p := range ix.keys[k] {
				out.Append(decodeIndexValue(L, ix.vals[p]))
			}
		}
		L.Push(out)
		return 1
	}))

	// mycelium.index.keys(name, prefix) → sorted array of the keys starting
	// with prefix ("" = all). For tasks walking an index, not for hot paths.
	t.RawSetString("keys", L.NewFunction(func(L *lua.LState) int {
		ix := pluginIndexes.get(plugin, L.CheckString(1))
		prefix := L.OptString(2, "")
		var ks []string
		if ix != nil {
			for k := range ix.keys {
				if strings.HasPrefix(k, prefix) {
					ks = append(ks, k)
				}
			}
		}
		sort.Strings(ks)
		if len(ks) > maxIndexKeysResult {
			ks = ks[:maxIndexKeysResult]
		}
		out := L.CreateTable(len(ks), 0)
		for _, k := range ks {
			out.Append(lua.LString(k))
		}
		L.Push(out)
		return 1
	}))

	// mycelium.index.info(name) → {records, keys, bytes, built_at} or nil
	t.RawSetString("info", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		ix := pluginIndexes.get(plugin, name)
		if ix == nil {
			L.Push(lua.LNil)
			return 1
		}
		in := ix.info(name)
		out := L.NewTable()
		out.RawSetString("records", lua.LNumber(in.Records))
		out.RawSetString("keys", lua.LNumber(in.Keys))
		out.RawSetString("bytes", lua.LNumber(in.Bytes))
		out.RawSetString("built_at", lua.LNumber(in.BuiltAt))
		L.Push(out)
		return 1
	}))

	// mycelium.index.drop(name)
	t.RawSetString("drop", L.NewFunction(func(L *lua.LState) int {
		pluginIndexes.drop(plugin, L.CheckString(1))
		return 0
	}))

	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// Streaming JSON arrays (mycelium.json.each, mycelium.storage.each_json)
// ─────────────────────────────────────────────────────────────────────────────

// streamJSONArray decodes the array at path (dot-separated object fields,
// "" = the document itself) one element at a time, calling each for every
// element; each returns false to stop. A missing path yields 0 elements.
func streamJSONArray(r io.Reader, path string, each func(v any) bool) (int, error) {
	dec := json.NewDecoder(r)
	if path != "" {
		for _, field := range strings.Split(path, ".") {
			found, err := seekJSONField(dec, field)
			if err != nil || !found {
				return 0, err
			}
		}
	}
	tok, err := dec.Token()
	if err != nil {
		return 0, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return 0, fmt.Errorf("json: expected an array at %q", path)
	}
	n := 0
	for dec.More() {
		var v any
		if err := dec.Decode(&v); err != nil {
			return n, err
		}
		n++
		if !each(v) {
			break
		}
	}
	return n, nil
}

// seekJSONField advances dec, positioned before an object, to the value of
// field (skipping the other members).
func seekJSONField(dec *json.Decoder, field string) (bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return false, nil
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return false, err
		}
		if k, _ := kt.(string); k == field {
			return true, nil
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return false, err
		}
	}
	return false, nil
}

// luaEachJSON runs streamJSONArray calling the Lua function fn(item, i) for
// every element; fn returning false stops. Errors raised by fn propagate.
func luaEachJSON(L *lua.LState, r io.Reader, path string, fn *lua.LFunction) (int, error) {
	i := 0
	return streamJSONArray(r, path, func(v any) bool {
		i++
		L.Push(fn)
		L.Push(goToLua(L, v))
		L.Push(lua.LNumber(i))
		L.Call(2, 1)
		ret := L.Get(-1)
		L.Pop(1)
		return ret != lua.LFalse
	})
}

// jsonEachFunc is mycelium.json.each(json_string, path, fn) → count, err.
func jsonEachFunc(L *lua.LState) int {
	s := L.CheckString(1)
	path := L.OptString(2, "")
	fn := L.CheckFunction(3)
	n, err := luaEachJSON(L, strings.NewReader(s), path, fn)
	L.Push(lua.LNumber(n))
	if err != nil {
		L.Push(lua.LString(err.Error()))
	} else {
		L.Push(lua.LNil)
	}
	return 2
}
