package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestPool(t *testing.T, size int) *LuaPool {
	t.Helper()
	script := filepath.Join(t.TempDir(), "init.lua")
	if err := os.WriteFile(script, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := NewLuaPool(size, script, nil, nil)
	if err != nil {
		t.Fatalf("pool init: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestBackgroundSlots(t *testing.T) {
	for size, want := range map[int]int{1: 1, 2: 1, 3: 2, 4: 3} {
		if got := backgroundSlots(size); got != want {
			t.Errorf("backgroundSlots(%d) = %d, want %d", size, got, want)
		}
	}
}

// The point of the reservation: with a background job holding its slot, the
// next background job waits, but a user-facing call still gets a state.
func TestLuaPool_BackgroundNeverTakesLastState(t *testing.T) {
	pool := newTestPool(t, 2)

	freeSlot, err := pool.BackgroundSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bgState, releaseBg, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = bgState

	// A second background job must queue behind the first...
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := pool.BackgroundSlot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second background slot: got %v, want deadline exceeded", err)
	}

	// ...while a user call gets the state that was left free.
	uctx, ucancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer ucancel()
	_, releaseUser, err := pool.Acquire(uctx)
	if err != nil {
		t.Fatalf("user acquire while a background job runs: %v", err)
	}
	releaseUser()

	// Once the background job is done its slot is reusable.
	releaseBg()
	freeSlot()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	free2, err := pool.BackgroundSlot(ctx2)
	if err != nil {
		t.Fatalf("background slot after release: %v", err)
	}
	free2()
}

func TestAcquireForCall_UserWaitIsBounded(t *testing.T) {
	old := userAcquireWait
	userAcquireWait = 50 * time.Millisecond
	defer func() { userAcquireWait = old }()

	p := &LuaPlugin{Pool: newTestPool(t, 1)}

	_, hold, _, _, err := acquireForCall(context.Background(), p, classInteractive)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, _, _, wait, err := acquireForCall(context.Background(), p, classInteractive)
	if err == nil || !strings.Contains(err.Error(), "plugin occupato") {
		t.Fatalf("got %v, want a 'plugin occupato' error", err)
	}
	if wait < 40*time.Millisecond || time.Since(start) > 2*time.Second {
		t.Errorf("waited %v (measured %v), want ~50ms", wait, time.Since(start))
	}

	hold()
	L, release, done, _, err := acquireForCall(context.Background(), p, classInteractive)
	if err != nil || L == nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release()
	done()
}

// A background call that fails to get a state must not leak its slot, and a
// finished one must give it back (the timeout path discards the LState but
// still runs done).
func TestAcquireForCall_BackgroundSlotNotLeaked(t *testing.T) {
	p := &LuaPlugin{Pool: newTestPool(t, 2)}

	_, release, done, _, err := acquireForCall(context.Background(), p, classBackground)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, _, err := acquireForCall(ctx, p, classBackground); err == nil {
		t.Fatal("second background call should have waited and timed out")
	}

	// Simulate the timeout path: LState not released, slot still freed.
	_ = release
	done()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	_, release3, done3, _, err := acquireForCall(ctx2, p, classBackground)
	if err != nil {
		t.Fatalf("slot leaked: %v", err)
	}
	release3()
	done3()
}

// The home's catalog burst has its own budget: it can hold all but one state,
// so a tap made while carousels load still finds a state, and — unlike
// background work — a long cron task doesn't stop the carousels.
// Catalog and background share ONE budget (size-1), not one each — see the
// doc comment on LuaPool.BackgroundSlot. A catalog fetch queues behind an
// in-flight background task on a 2-state pool.
func TestAcquireForCall_CatalogAndBackgroundShareOneBudget(t *testing.T) {
	p := &LuaPlugin{Pool: newTestPool(t, 2)}

	// A long background task holds the plugin's one non-interactive slot.
	_, _, doneBg, _, err := acquireForCall(context.Background(), p, classBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer doneBg()

	// A catalog fetch must wait for it — the two classes are not independent.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, _, err := acquireForCall(ctx, p, classCatalog); err == nil {
		t.Fatal("catalog fetch should have queued behind the background task")
	}
}

// The bug this pins: two INDEPENDENT "leave one free" budgets (one per
// class) can each be satisfied at once, jointly consuming every state of a
// small pool and starving an interactive caller despite each class
// individually respecting its own rule. With a 2-state pool a background
// task and a catalog fetch running "at the same time" must not both get a
// state.
func TestAcquireForCall_CombinedNonInteractiveNeverStarvesInteractive(t *testing.T) {
	p := &LuaPlugin{Pool: newTestPool(t, 2)}

	_, _, doneBg, _, err := acquireForCall(context.Background(), p, classBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer doneBg()

	// The second non-interactive class (catalog) must NOT also get in while
	// background already holds the shared slot...
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, _, err := acquireForCall(ctx, p, classCatalog); err == nil {
		t.Fatal("catalog must not run concurrently with background — that's the exact bug")
	}

	// ...so an interactive call still finds its state.
	uctx, ucancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer ucancel()
	_, release, done, _, err := acquireForCall(uctx, p, classInteractive)
	if err != nil {
		t.Fatalf("interactive starved by background+catalog together: %v", err)
	}
	release()
	done()
}

func TestAcquireForCall_InteractiveNotStarvedByCatalogBurst(t *testing.T) {
	p := &LuaPlugin{Pool: newTestPool(t, 2)}

	_, releaseCat, doneCat, _, err := acquireForCall(context.Background(), p, classCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { releaseCat(); doneCat() }()

	// Ten more carousels queue up for the catalog slot...
	for i := 0; i < 10; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if _, r, d, _, err := acquireForCall(ctx, p, classCatalog); err == nil {
				r()
				d()
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)

	// ...and the tap that follows still gets the state they can't take.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, release, done, _, err := acquireForCall(ctx, p, classInteractive)
	if err != nil {
		t.Fatalf("interactive call behind a catalog burst: %v", err)
	}
	release()
	done()
}

func TestClassOf(t *testing.T) {
	cases := map[string]callClass{
		EPGetCatalog:       classCatalog,
		EPGetCatalogList:   classCatalog,
		EPGetDetails:       classInteractive,
		EPSearch:           classInteractive,
		EPGetStreams:       classInteractive,
		EPResolveStream:    classInteractive,
		"anything_else":    classInteractive,
		EPGetSearchFilters: classInteractive,
	}
	for ep, want := range cases {
		if got := classOf(ep); got != want {
			t.Errorf("classOf(%q) = %v, want %v", ep, got, want)
		}
	}
}

func TestEntrypointTimeout_ReadsAreCapped_ResolveKeepsDefault(t *testing.T) {
	mf := LuaManifest{ID: "p"}
	for _, ep := range []string{EPSearch, EPGetSearchFilters, EPGetDetails, EPBrowse, EPGetStreams} {
		if got := entrypointTimeout(mf, ep, "fn"); got != readEntrypointTimeout {
			t.Errorf("entrypointTimeout(%s) = %v, want %v", ep, got, readEntrypointTimeout)
		}
	}
	if got := entrypointTimeout(mf, EPResolveStream, "resolve_stream"); got != defaultEntrypointTimeout {
		t.Errorf("resolve must keep the long default, got %v", got)
	}
	if got := entrypointTimeout(mf, EPGetCatalog, "get_catalog"); got != defaultEntrypointTimeout {
		t.Errorf("catalog must keep the default, got %v", got)
	}
}
