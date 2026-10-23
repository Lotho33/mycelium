package pileus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/engine"
)

// fakeResolve is a resolveRun that blocks until release is closed, counting
// its calls and emitting one progress event when it starts.
type fakeResolve struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	err     error
	ctxErr  atomic.Value // error seen on ctx once released
}

func newFakeResolve() *fakeResolve {
	return &fakeResolve{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (f *fakeResolve) run(ctx context.Context, onProgress engine.ProgressFunc) (*ResolvedStream, error) {
	f.calls.Add(1)
	onProgress("", "sniff…")
	f.started <- struct{}{}
	select {
	case <-f.release:
	case <-ctx.Done():
		f.ctxErr.Store(ctx.Err())
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		f.ctxErr.Store(err)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &ResolvedStream{URL: "https://cdn.example/x.m3u8"}, nil
}

func waitFlightsEmpty(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for resolveFlights.size() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("flights still running: %d", resolveFlights.size())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitCallers waits until n callers are on the flight of (p, streamID).
func waitCallers(t *testing.T, streamID string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for resolveFlights.callers(resolveFlightKey("p", streamID)) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s: callers never reached %d", streamID, n)
		}
		time.Sleep(time.Millisecond)
	}
}

type progressLog struct {
	mu  sync.Mutex
	got []string
}

func (p *progressLog) fn(_, msg string) {
	p.mu.Lock()
	p.got = append(p.got, msg)
	p.mu.Unlock()
}

func (p *progressLog) list() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}

// Two callers, one backend resolve; the follower gets the progress replayed.
func TestResolveShared_TwoCallersOneResolve(t *testing.T) {
	f := newFakeResolve()
	ctx := context.Background()

	type res struct {
		rs     *ResolvedStream
		joined bool
		err    error
	}
	leaderDone := make(chan res, 1)
	go func() {
		rs, joined, _, err := resolveShared(ctx, "p", "s1", false, nil, f.run)
		leaderDone <- res{rs, joined, err}
	}()
	<-f.started

	var follower progressLog
	followerDone := make(chan res, 1)
	go func() {
		rs, joined, _, err := resolveShared(ctx, "p", "s1", false, follower.fn, f.run)
		followerDone <- res{rs, joined, err}
	}()
	// The follower is subscribed once the replay arrives.
	deadline := time.Now().Add(time.Second)
	for len(follower.list()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("follower got no progress replay")
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(f.release)

	l, fo := <-leaderDone, <-followerDone
	if l.err != nil || fo.err != nil {
		t.Fatalf("errors: %v / %v", l.err, fo.err)
	}
	if l.joined || !fo.joined {
		t.Fatalf("joined: leader=%v follower=%v", l.joined, fo.joined)
	}
	if l.rs != fo.rs {
		t.Fatal("follower did not get the leader's result")
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("backend resolves = %d, want 1", n)
	}
	if got := follower.list(); got[0] != "sniff…" {
		t.Fatalf("replay = %v", got)
	}
	waitFlightsEmpty(t)
}

// A cancelled follower stops waiting; the leader still gets the result.
func TestResolveShared_FollowerCancelDoesNotCancelLeader(t *testing.T) {
	f := newFakeResolve()
	leaderDone := make(chan error, 1)
	go func() {
		_, _, _, err := resolveShared(context.Background(), "p", "s2", false, nil, f.run)
		leaderDone <- err
	}()
	<-f.started

	fctx, cancel := context.WithCancel(context.Background())
	followerDone := make(chan error, 1)
	go func() {
		_, _, _, err := resolveShared(fctx, "p", "s2", false, nil, f.run)
		followerDone <- err
	}()
	waitCallers(t, "s2", 2)
	cancel()
	if err := <-followerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("follower err = %v, want Canceled", err)
	}
	close(f.release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader err = %v", err)
	}
	waitFlightsEmpty(t)
}

// A warm leader going away does not stop the resolve: a later joiner still
// gets its result. (A real warm's ctx is never cancelled; this is the
// guarantee in case it were.)
func TestResolveShared_WarmLeaderCancelKeepsWorking(t *testing.T) {
	f := newFakeResolve()
	lctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, _, _, err := resolveShared(lctx, "p", "s3", true, nil, f.run)
		leaderDone <- err
	}()
	<-f.started
	cancel()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v, want Canceled", err)
	}
	if !resolveFlights.inFlight(resolveFlightKey("p", "s3")) {
		t.Fatal("flight dropped when its leader went away")
	}
	joinDone := make(chan error, 1)
	go func() {
		rs, joined, _, err := resolveShared(context.Background(), "p", "s3", true, nil, f.run)
		if err == nil && (!joined || rs == nil) {
			err = errors.New("not joined")
		}
		joinDone <- err
	}()
	waitCallers(t, "s3", 2)
	close(f.release)
	if err := <-joinDone; err != nil {
		t.Fatal(err)
	}
	if e := f.ctxErr.Load(); e != nil {
		t.Fatalf("resolve ctx was cancelled: %v", e)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("backend resolves = %d, want 1", n)
	}
	waitFlightsEmpty(t)
}

// An error reaches every caller.
func TestResolveShared_ErrorToAll(t *testing.T) {
	f := newFakeResolve()
	f.err = errors.New("boom")
	errs := make(chan error, 2)
	go func() {
		_, _, _, err := resolveShared(context.Background(), "p", "s4", false, nil, f.run)
		errs <- err
	}()
	<-f.started
	go func() {
		_, _, _, err := resolveShared(context.Background(), "p", "s4", false, nil, f.run)
		errs <- err
	}()
	waitCallers(t, "s4", 2)
	close(f.release)
	for range 2 {
		if err := <-errs; err == nil || err.Error() != "boom" {
			t.Fatalf("err = %v, want boom", err)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("backend resolves = %d, want 1", n)
	}
	waitFlightsEmpty(t)
}

// A Play that joined a warm runs once more after it (a cache hit in the
// plugin), so per-profile writes still happen; another Play joining a Play
// does not.
func TestResolveShared_PlayAfterWarmRunsAgain(t *testing.T) {
	f := newFakeResolve()
	warmDone := make(chan error, 1)
	go func() {
		_, _, _, err := resolveShared(context.Background(), "p", "s5", true, nil, f.run)
		warmDone <- err
	}()
	<-f.started
	playDone := make(chan error, 1)
	go func() {
		_, joined, _, err := resolveShared(context.Background(), "p", "s5", false, nil, f.run)
		if err == nil && !joined {
			err = errors.New("play did not join the warm")
		}
		playDone <- err
	}()
	waitCallers(t, "s5", 2)
	close(f.release) // both runs return at once from here on
	if err := <-warmDone; err != nil {
		t.Fatal(err)
	}
	if err := <-playDone; err != nil {
		t.Fatal(err)
	}
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("backend resolves = %d, want 2 (warm + the Play's cache hit)", n)
	}
	waitFlightsEmpty(t)
}

// A panicking backend fails the flight instead of leaving it stuck.
func TestResolveShared_PanicFailsFlight(t *testing.T) {
	_, _, _, err := resolveShared(context.Background(), "p", "s6", false, nil,
		func(context.Context, engine.ProgressFunc) (*ResolvedStream, error) { panic("lua exploded") })
	if err == nil {
		t.Fatal("want an error from a panicking resolve")
	}
	waitFlightsEmpty(t)
}

// A Play that every caller abandoned is cancelled, and leaves the map at
// once: the next Play starts a fresh resolve instead of joining a dying one.
func TestResolveShared_AbandonedPlayIsCancelled(t *testing.T) {
	f := newFakeResolve()
	pctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := resolveShared(pctx, "p", "s7", false, nil, f.run)
		done <- err
	}()
	<-f.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	if resolveFlights.inFlight(resolveFlightKey("p", "s7")) {
		t.Fatal("abandoned flight still joinable")
	}
	waitFlightsEmpty(t)
	deadline := time.Now().Add(2 * time.Second)
	for f.ctxErr.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("abandoned resolve was not cancelled")
		}
		time.Sleep(time.Millisecond)
	}
}

// A Play leader going away while another caller still waits keeps the
// resolve running for that caller.
func TestResolveShared_PlayLeaderCancelWithFollowerKeepsWorking(t *testing.T) {
	f := newFakeResolve()
	lctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, _, _, err := resolveShared(lctx, "p", "s8", false, nil, f.run)
		leaderDone <- err
	}()
	<-f.started
	followerDone := make(chan error, 1)
	go func() {
		_, _, _, err := resolveShared(context.Background(), "p", "s8", false, nil, f.run)
		followerDone <- err
	}()
	waitCallers(t, "s8", 2)
	cancel()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v", err)
	}
	close(f.release)
	if err := <-followerDone; err != nil {
		t.Fatalf("follower err = %v", err)
	}
	if e := f.ctxErr.Load(); e != nil {
		t.Fatalf("resolve cancelled with a caller still waiting: %v", e)
	}
	waitFlightsEmpty(t)
}

// A warm joining a Play's flight keeps it alive when the Play backs out:
// the cache still gets filled.
func TestResolveShared_WarmJoinerKeepsPlayFlightAlive(t *testing.T) {
	f := newFakeResolve()
	pctx, cancel := context.WithCancel(context.Background())
	playDone := make(chan error, 1)
	go func() {
		_, _, _, err := resolveShared(pctx, "p", "s9", false, nil, f.run)
		playDone <- err
	}()
	<-f.started
	warmDone := make(chan error, 1)
	go func() {
		_, _, _, err := resolveShared(context.Background(), "p", "s9", true, nil, f.run)
		warmDone <- err
	}()
	waitCallers(t, "s9", 2)
	cancel()
	<-playDone
	close(f.release)
	if err := <-warmDone; err != nil {
		t.Fatalf("warm err = %v", err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("resolves = %d, want 1", n)
	}
	waitFlightsEmpty(t)
}
