package api

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// segAcquire caps concurrent holders per host and unblocks the next waiter when
// one releases; a cancelled context stops waiting instead of blocking forever.
func TestSegAcquire_CapsAndReleases(t *testing.T) {
	t.Setenv("MYCELIUM_SEGMENT_CONCURRENCY", "2")
	const host = "segtest.example"
	segGateMu.Lock()
	delete(segGates, host) // fresh gate at the env'd size
	segGateMu.Unlock()

	ctx := context.Background()
	r1, err := segAcquire(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := segAcquire(ctx, host)
	if err != nil {
		t.Fatal(err)
	}

	// Third acquire must block until a slot frees.
	got := make(chan struct{})
	go func() {
		r3, e := segAcquire(ctx, host)
		if e == nil {
			r3()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("third segAcquire returned while 2/2 slots held")
	case <-time.After(100 * time.Millisecond):
	}
	r1()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("third segAcquire did not proceed after a release")
	}
	r2()
}

func TestSegAcquire_ContextCancel(t *testing.T) {
	t.Setenv("MYCELIUM_SEGMENT_CONCURRENCY", "1")
	const host = "segcancel.example"
	segGateMu.Lock()
	delete(segGates, host)
	segGateMu.Unlock()

	rel, err := segAcquire(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	ctx, cancel := context.WithCancel(context.Background())
	var got atomic.Value
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, e := segAcquire(ctx, host); got.Store(e != nil) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	wg.Wait()
	if v, _ := got.Load().(bool); !v {
		t.Fatal("segAcquire ignored context cancellation")
	}
}

// A plain 403/429 is rate-limiting → back off seconds; anything else keeps the
// fast millisecond retry.
func TestSegRetryBackoff(t *testing.T) {
	if d := segRetryBackoff(1, http.StatusForbidden); d < time.Second {
		t.Errorf("403 backoff = %s, want ≥1s", d)
	}
	if d := segRetryBackoff(10, http.StatusTooManyRequests); d > 4*time.Second {
		t.Errorf("429 backoff = %s, want capped at 4s", d)
	}
	if d := segRetryBackoff(1, http.StatusBadGateway); d > 500*time.Millisecond {
		t.Errorf("502 backoff = %s, want the fast path", d)
	}
}
