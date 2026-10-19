package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// indexTestState is a bare LState with mycelium.index/json/storage for
// plugin id, storage rooted at dir.
func indexTestState(t *testing.T, id, dir string) *lua.LState {
	t.Helper()
	L := lua.NewState()
	t.Cleanup(L.Close)
	opts := SDKOpts{PluginID: id, PluginDir: dir}
	m := L.NewTable()
	m.RawSetString("index", buildIndexModule(L, opts))
	m.RawSetString("json", buildJSONModule(L))
	m.RawSetString("storage", buildStorageModule(L, opts))
	L.SetGlobal("mycelium", m)
	return L
}

func runLua(t *testing.T, L *lua.LState, code string) {
	t.Helper()
	if err := L.DoString(code); err != nil {
		t.Fatal(err)
	}
}

func TestIndex_BuildPublishLookup(t *testing.T) {
	t.Cleanup(func() { pluginIndexes.dropPlugin("ix.a"); pluginIndexes.dropPlugin("ix.b") })
	a1 := indexTestState(t, "ix.a", t.TempDir())
	a2 := indexTestState(t, "ix.a", t.TempDir())
	b := indexTestState(t, "ix.b", t.TempDir())

	runLua(t, a1, `
		assert(not mycelium.index.has("cat"))
		local b = mycelium.index.builder()
		b:add({ id = 1, name = "uno" }, "id:1", "mal:10")
		b:add({ id = 2, name = "due" }, "id:2", "mal:10", "")
		b:add({ id = 1, name = "uno" }, "alias:uno")   -- same value: stored once
		b:add(7, "n:" .. 7)
		assert(b:count() == 3)
		local n, err = b:publish("cat")
		assert(n == 3 and err == nil)
		local _, again = b:publish("cat")
		assert(again ~= nil)
	`)
	// Another state of the same plugin sees it; another plugin does not.
	runLua(t, a2, `
		assert(mycelium.index.has("cat"))
		assert(mycelium.index.get("cat", "id:2").name == "due")
		assert(mycelium.index.get("cat", "mal:10").id == 1)    -- first added wins
		local all = mycelium.index.get_all("cat", "mal:10")
		assert(#all == 2 and all[2].id == 2)
		assert(mycelium.index.get("cat", "alias:uno").id == 1)
		assert(mycelium.index.get("cat", "n:7") == 7)
		assert(mycelium.index.get("cat", "missing") == nil)
		assert(#mycelium.index.get_all("cat", "missing") == 0)
		local ks = mycelium.index.keys("cat", "id:")
		assert(#ks == 2 and ks[1] == "id:1" and ks[2] == "id:2")
		local info = mycelium.index.info("cat")
		assert(info.records == 3 and info.keys == 5 and info.bytes > 0)
		-- a returned value is a copy: changing it changes nothing shared
		mycelium.index.get("cat", "id:1").name = "x"
		assert(mycelium.index.get("cat", "id:1").name == "uno")
	`)
	runLua(t, b, `assert(not mycelium.index.has("cat"))`)

	// Numbers and strings name the same key.
	runLua(t, a1, `
		local b = mycelium.index.builder()
		b:add("x", 123)
		b:publish("num")
		assert(mycelium.index.get("num", "123") == "x")
		assert(mycelium.index.get("num", 123) == "x")
		mycelium.index.drop("num")
		assert(not mycelium.index.has("num"))
	`)

	if infos := pluginIndexes.infos("ix.a"); len(infos) != 1 || infos[0].Name != "cat" {
		t.Fatalf("infos = %+v", infos)
	}
	pluginIndexes.dropPlugin("ix.a")
	runLua(t, a2, `assert(not mycelium.index.has("cat"))`)
}

func TestIndex_TooManyIndexes(t *testing.T) {
	t.Cleanup(func() { pluginIndexes.dropPlugin("ix.many") })
	L := indexTestState(t, "ix.many", t.TempDir())
	runLua(t, L, `
		for i = 1, 16 do
			local b = mycelium.index.builder(); b:add(1, "k")
			assert(b:publish("i" .. i))
		end
		local b = mycelium.index.builder(); b:add(1, "k")
		local n, err = b:publish("one-too-many")
		assert(n == nil and err ~= nil)
		-- replacing an existing one is still fine
		b = mycelium.index.builder(); b:add(2, "k")
		assert(b:publish("i1") == 1)
		assert(mycelium.index.get("i1", "k") == 2)
	`)
}

func TestJSONEach_StreamsArrays(t *testing.T) {
	dir := t.TempDir()
	doc := `{"meta":{"v":1},"lists":{"al":[{"id":1},{"id":2},{"id":3}],"mal":[]},"tail":[9]}`
	if err := os.WriteFile(filepath.Join(dir, "f.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	L := indexTestState(t, "ix.each", dir)
	L.SetGlobal("DOC", lua.LString(doc))
	runLua(t, L, `
		local seen = {}
		local n, err = mycelium.json.each(DOC, "lists.al", function(e, i) seen[#seen+1] = e.id * 10 + i end)
		assert(n == 3 and err == nil, tostring(err))
		assert(seen[1] == 11 and seen[3] == 33)

		-- false stops early
		n = mycelium.json.each(DOC, "lists.al", function(e) return e.id < 2 end)
		assert(n == 2)

		assert(mycelium.json.each(DOC, "lists.mal", function() error("never") end) == 0)
		assert(mycelium.json.each(DOC, "nope.al", function() end) == 0)
		local _, e2 = mycelium.json.each(DOC, "meta", function() end)
		assert(e2 ~= nil)   -- not an array
		assert(mycelium.json.each("[1,2]", nil, function() end) == 2)

		n, err = mycelium.storage.each_json("f.json", "tail", function(v) assert(v == 9) end)
		assert(n == 1 and err == nil)
		n, err = mycelium.storage.each_json("missing.json", nil, function() end)
		assert(n == 0 and err == nil)
		n, err = mycelium.storage.each_json("../x.json", nil, function() end)
		assert(err ~= nil)
	`)
	// An error raised by the callback propagates.
	err := L.DoString(`mycelium.json.each("[1]", nil, function() error("boom") end)`)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}
