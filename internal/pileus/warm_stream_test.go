package pileus

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// warmTestEnv swaps the WarmStream seams and state for one test: every check
// passes and the resolve is f.run.
func warmTestEnv(t *testing.T, f *fakeResolve) *struct{ enabled, browserBusy bool } {
	t.Helper()
	env := &struct{ enabled, browserBusy bool }{enabled: true}
	oldEnabled, oldBusy, oldRun, oldPerHour := warmEnabled, warmBrowserBusy, warmRunFor, warmPerHour
	oldWarmer, oldStats := warmer, warmStats
	warmEnabled = func(string) bool { return env.enabled }
	warmBrowserBusy = func() bool { return env.browserBusy }
	warmRunFor = func(string, string) resolveRun { return f.run }
	warmPerHour = func() float64 { return 20 }
	warmer = newWarmState()
	warmStats = &warmMetrics{skipped: map[string]int64{}}
	t.Cleanup(func() {
		warmEnabled, warmBrowserBusy, warmRunFor, warmPerHour = oldEnabled, oldBusy, oldRun, oldPerHour
		warmer, warmStats = oldWarmer, oldStats
	})
	return env
}

func warmCtx() context.Context {
	return context.WithValue(context.WithValue(context.Background(), ctxKeyDeviceID{}, "tv"), ctxKeyProfileID{}, "me")
}

func warmCall(t *testing.T, streamID string) *gen.WarmStreamResponse {
	t.Helper()
	resp, err := (&MediaHandler{}).WarmStream(warmCtx(), &gen.WarmStreamRequest{PluginId: "p", StreamId: streamID})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func wantWarm(t *testing.T, resp *gen.WarmStreamResponse, outcome, reason string) {
	t.Helper()
	if resp.Outcome != outcome || resp.Reason != reason {
		t.Fatalf("got %s(%s), want %s(%s)", resp.Outcome, resp.Reason, outcome, reason)
	}
}

// waitWarmDone waits for the warm goroutine to finish (it frees the slot).
func waitWarmDone(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if release, ok := warmer.tryAcquire(); ok {
			release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("warm never finished")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWarmStream_InvalidArgument(t *testing.T) {
	warmTestEnv(t, newFakeResolve())
	_, err := (&MediaHandler{}).WarmStream(warmCtx(), &gen.WarmStreamRequest{PluginId: "p"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v", err)
	}
}

func TestWarmStream_Disabled(t *testing.T) {
	env := warmTestEnv(t, newFakeResolve())
	env.enabled = false
	wantWarm(t, warmCall(t, "w1"), warmSkipped, warmReasonDisabled)
}

// started: the resolve runs in the background, takes no playback lease and
// registers no session; a second call for the same stream is "recent".
func TestWarmStream_StartedNoLeaseNoSession(t *testing.T) {
	setupPinTestDB(t)
	defer managers.PlaybackLeases.Release("me")
	f := newFakeResolve()
	warmTestEnv(t, f)

	wantWarm(t, warmCall(t, "w2"), warmStarted, "")
	<-f.started
	if holder := managers.PlaybackLeases.Holder("me", "phone"); holder != "" {
		t.Fatalf("warm took the playback lease (holder %q)", holder)
	}
	if managers.Sessions.ActiveWithin(time.Minute) {
		t.Fatal("warm registered a playback session")
	}
	// In flight: joined, nothing new starts.
	wantWarm(t, warmCall(t, "w2"), warmJoined, "")
	close(f.release)
	waitWarmDone(t)
	waitFlightsEmpty(t)

	if n := f.calls.Load(); n != 1 {
		t.Fatalf("resolves = %d, want 1", n)
	}
	if warmStats.completedOK.Load() != 1 {
		t.Fatal("completion not counted")
	}
	wantWarm(t, warmCall(t, "w2"), warmSkipped, warmReasonRecent)
}

// One warm at a time server-wide.
func TestWarmStream_BusyWhileAnotherWarm(t *testing.T) {
	f := newFakeResolve()
	warmTestEnv(t, f)
	wantWarm(t, warmCall(t, "w3"), warmStarted, "")
	<-f.started
	wantWarm(t, warmCall(t, "w4"), warmSkipped, warmReasonBusy)
	close(f.release)
	waitWarmDone(t)
	waitFlightsEmpty(t)
}

// No warm while a real Play is resolving.
func TestWarmStream_BusyWhilePlayResolving(t *testing.T) {
	warmTestEnv(t, newFakeResolve())
	resolvesInFlight.Add(1)
	defer resolvesInFlight.Add(-1)
	wantWarm(t, warmCall(t, "w5"), warmSkipped, warmReasonBusy)
	// The slot taken for the check went back.
	if release, ok := warmer.tryAcquire(); !ok {
		t.Fatal("warm slot leaked")
	} else {
		release()
	}
}

func TestWarmStream_BrowserBusy(t *testing.T) {
	env := warmTestEnv(t, newFakeResolve())
	env.browserBusy = true
	wantWarm(t, warmCall(t, "w6"), warmSkipped, warmReasonBrowserBusy)
}

func TestWarmStream_Budget(t *testing.T) {
	f := newFakeResolve()
	close(f.release) // every resolve returns at once
	warmTestEnv(t, f)
	warmPerHour = func() float64 { return 2 }
	for _, id := range []string{"b1", "b2"} {
		wantWarm(t, warmCall(t, id), warmStarted, "")
		waitWarmDone(t)
	}
	wantWarm(t, warmCall(t, "b3"), warmSkipped, warmReasonBudget)
	if warmStats.skipped[warmReasonBudget] != 1 {
		t.Fatalf("skipped = %v", warmStats.skipped)
	}
}

// A Play of a warmed stream counts as a warm hit, once.
func TestWarmStream_HitCountedOnce(t *testing.T) {
	warmTestEnv(t, newFakeResolve())
	key := resolveFlightKey("p", "h1")
	warmer.markCompleted(key)
	if !warmer.takeHit(key) || warmer.takeHit(key) {
		t.Fatal("a warmed stream must count as a hit exactly once")
	}
}

// A Play that joins a warm still running counts as a warm hit (in flight),
// once, and runs its own resolve after it (the plugin's cache hit).
func TestWarmStream_PlayJoiningWarmIsAHit(t *testing.T) {
	f := newFakeResolve()
	warmTestEnv(t, f)
	wantWarm(t, warmCall(t, "h2"), warmStarted, "")
	<-f.started
	playDone := make(chan bool, 1)
	go func() {
		_, joined, joinedWarm, err := resolveShared(context.Background(), "p", "h2", false, nil, f.run)
		playDone <- err == nil && joined && joinedWarm
	}()
	waitCallers(t, "h2", 2)
	close(f.release)
	if !<-playDone {
		t.Fatal("Play did not join the warm")
	}
	// What ResolveStream checks once its resolve is back.
	if !warmer.takeHit(resolveFlightKey("p", "h2")) {
		t.Fatal("warm not completed by the time the joined Play returned")
	}
	if warmer.takeHit(resolveFlightKey("p", "h2")) {
		t.Fatal("hit counted twice")
	}
	waitWarmDone(t)
	waitFlightsEmpty(t)
}

// A warm that found no free Lua state reached no site: it may be retried
// at once instead of being "recent" for 30 minutes. Other failures stay
// recent, and never count as a hit.
func TestWarmStream_BusyFailureNotRecent(t *testing.T) {
	f := newFakeResolve()
	close(f.release)
	warmTestEnv(t, f)
	f.err = fmt.Errorf("plugin %q: %w", "p", engine.ErrPluginBusy)
	wantWarm(t, warmCall(t, "r1"), warmStarted, "")
	waitWarmDone(t)
	wantWarm(t, warmCall(t, "r1"), warmStarted, "")
	waitWarmDone(t)

	f.err = errors.New("site down")
	wantWarm(t, warmCall(t, "r2"), warmStarted, "")
	waitWarmDone(t)
	wantWarm(t, warmCall(t, "r2"), warmSkipped, warmReasonRecent)
	if warmer.takeHit(resolveFlightKey("p", "r2")) {
		t.Fatal("a failed warm must not count as a hit")
	}
}
