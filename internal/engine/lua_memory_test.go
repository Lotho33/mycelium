package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

func heapNow() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// The estimate must stay in the same order of magnitude as the real Go heap
// for the shapes plugins actually build (string-keyed records, arrays).
func TestProfileState_CalibratedAgainstHeap(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	before := heapNow()
	if err := L.DoString(`
		recs = {}
		for i = 1, 50000 do
			recs[i] = { id = i, title = "Titolo numero " .. i, slug = "titolo-" .. i, year = 2000 + i % 25 }
		end
		byslug = {}
		for i = 1, 50000 do byslug["titolo-" .. i] = recs[i] end
	`); err != nil {
		t.Fatal(err)
	}
	real := heapNow() - before
	_, items := profileState(context.Background(), L)
	var est int64
	for _, it := range items {
		if it.Path == "global recs" || it.Path == "global byslug" {
			est += it.Bytes
		}
	}
	ratio := float64(est) / float64(real)
	t.Logf("heap reale %d KB, stima %d KB, rapporto %.2f", real>>10, est>>10, ratio)
	if ratio < 0.5 || ratio > 2 {
		t.Fatalf("stima fuori scala: rapporto %.2f", ratio)
	}
}

// A module-level local captured by global functions is reported by name,
// and the globals table isn't swallowed by the first function reached.
func TestProfileState_AttributesModuleLocals(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	if err := L.DoString(`
		local cache = {}
		for i = 1, 5000 do cache[i] = { name = "x" .. i } end
		function get(i) return cache[i] end
		small = { a = 1 }
	`); err != nil {
		t.Fatal(err)
	}
	st, items := profileState(context.Background(), L)
	if len(items) == 0 || items[0].Path != "local cache (init.lua)" {
		t.Fatalf("top item = %+v, want local cache first", items)
	}
	for _, it := range items {
		if it.Path == "global small" && it.Entries != 1 {
			t.Fatalf("global small = %+v, want its own single entry", it)
		}
	}
	if st.Bytes <= items[0].Bytes || st.Truncated {
		t.Fatalf("state total %+v vs top %+v", st, items[0])
	}
}

func TestLuaPool_ProfileMeasuresEveryIdleState(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "init.lua")
	if err := os.WriteFile(script, []byte(`
		local idx = {}
		for i = 1, 2000 do idx["k" .. i] = i end
		function lookup(k) return idx[k] end
	`), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := NewLuaPool(2, script, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	states, items, busy := pool.Profile(context.Background(), time.Second)
	if len(states) != 2 || busy != 0 {
		t.Fatalf("states=%d busy=%d, want 2 measured", len(states), busy)
	}
	if !strings.HasPrefix(items[0].Path, "local idx") || items[0].Entries != 4000 {
		t.Fatalf("top = %+v, want local idx summed over both states", items[0])
	}
	if free, _ := pool.Stats(); free != 2 {
		t.Fatalf("pool has %d free states after Profile, want 2 (all returned)", free)
	}

	// A state held by a call is reported busy, not waited on forever.
	L, release, _ := pool.Acquire(context.Background())
	_ = L
	states, _, busy = pool.Profile(context.Background(), 50*time.Millisecond)
	release()
	if len(states) != 1 || busy != 1 {
		t.Fatalf("with one state in use: measured=%d busy=%d, want 1/1", len(states), busy)
	}
}
