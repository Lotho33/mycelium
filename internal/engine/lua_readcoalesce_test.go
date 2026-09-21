package engine

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadCoalescer_ConcurrentIdenticalCallsRunOnce(t *testing.T) {
	var c readCoalescer
	var runs atomic.Int32
	release := make(chan struct{})

	fn := func() (json.RawMessage, error) {
		runs.Add(1)
		<-release
		return json.RawMessage(`{"ok":1}`), nil
	}

	var wg sync.WaitGroup
	results := make([]string, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.do("k", fn)
			if err != nil {
				t.Errorf("call %d: %v", i, err)
			}
			results[i] = string(v)
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let every caller join the flight
	close(release)
	wg.Wait()

	if n := runs.Load(); n != 1 {
		t.Fatalf("fn ran %d times, want 1", n)
	}
	for i, r := range results {
		if r != `{"ok":1}` {
			t.Errorf("caller %d got %q", i, r)
		}
	}
}

func TestReadCoalescer_SuccessIsCachedThenExpires(t *testing.T) {
	var c readCoalescer
	var runs atomic.Int32
	fn := func() (json.RawMessage, error) {
		runs.Add(1)
		return json.RawMessage(`[1]`), nil
	}
	for i := 0; i < 3; i++ {
		if _, err := c.do("k", fn); err != nil {
			t.Fatal(err)
		}
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("after 3 calls fn ran %d times, want 1 (cached)", n)
	}

	c.mu.Lock()
	e := c.cache["k"]
	e.exp = time.Now().Add(-time.Second)
	c.cache["k"] = e
	c.mu.Unlock()

	if _, err := c.do("k", fn); err != nil {
		t.Fatal(err)
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("after expiry fn ran %d times, want 2", n)
	}
}

func TestReadCoalescer_ErrorsAndNullAreNotCached(t *testing.T) {
	var c readCoalescer
	var runs atomic.Int32

	boom := func() (json.RawMessage, error) { runs.Add(1); return nil, errors.New("boom") }
	for i := 0; i < 2; i++ {
		if _, err := c.do("e", boom); err == nil {
			t.Fatal("want error")
		}
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("failing call ran %d times, want 2 (errors must not stick)", n)
	}

	runs.Store(0)
	null := func() (json.RawMessage, error) { runs.Add(1); return json.RawMessage("null"), nil }
	for i := 0; i < 2; i++ {
		if _, err := c.do("n", null); err != nil {
			t.Fatal(err)
		}
	}
	if n := runs.Load(); n != 2 {
		t.Fatalf("'null' (not found) ran %d times, want 2 (must not be cached)", n)
	}
}

func TestReadCoalescer_PanicDoesNotWedgeTheKey(t *testing.T) {
	var c readCoalescer
	if _, err := c.do("p", func() (json.RawMessage, error) { panic("x") }); err == nil {
		t.Fatal("want the panic reported as an error")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := c.do("p", func() (json.RawMessage, error) { return json.RawMessage(`1`), nil })
		if err != nil || string(v) != "1" {
			t.Errorf("after a panic: %q, %v", v, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("key stayed wedged after a panicking call")
	}
}

func TestReadCoalescer_ReturnsIndependentCopies(t *testing.T) {
	var c readCoalescer
	fn := func() (json.RawMessage, error) { return json.RawMessage(`"abc"`), nil }
	a, _ := c.do("k", fn)
	a[1] = 'X' // a caller scribbling on its copy...
	b, _ := c.do("k", fn)
	if string(b) != `"abc"` {
		t.Fatalf("cached value was corrupted through a caller's copy: %q", b)
	}
}

func TestReadKey(t *testing.T) {
	k1, _ := readKey("p", "details", "prof", map[string]any{"a": 1, "b": "x"})
	k2, _ := readKey("p", "details", "prof", map[string]any{"b": "x", "a": 1})
	if k1 != k2 {
		t.Error("same args in a different order must give the same key")
	}
	k3, _ := readKey("p", "details", "other-profile", map[string]any{"a": 1, "b": "x"})
	if k1 == k3 {
		t.Error("different profile must give a different key")
	}
}

func TestCoalescibleRead(t *testing.T) {
	for _, ep := range []string{EPSearch, EPGetSearchFilters, EPGetDetails, EPBrowse, EPGetStreams} {
		if !coalescibleRead(ep) {
			t.Errorf("%s should coalesce", ep)
		}
	}
	for _, ep := range []string{EPResolveStream, EPGetCatalog, EPGetCatalogList} {
		if coalescibleRead(ep) {
			t.Errorf("%s must not coalesce", ep)
		}
	}
}
