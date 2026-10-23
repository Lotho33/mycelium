package pileus

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"mycelium/internal/engine"
)

// Coalescing of stream resolves. One plugin resolve per (plugin, stream) is
// in flight at a time: a Play that arrives while a warm (WarmStream) or
// another device's Play is resolving the same stream joins it instead of
// starting a second extraction.
//
// The resolve runs detached from whoever started it, and each caller waits
// on its own ctx: a cancelled caller only stops waiting. A resolve started
// by a warm always finishes and fills the plugin's resolve cache (that is
// its point). One started by a Play is cancelled once every caller has gone
// (the user backed out): left running, an abandoned extraction would hold an
// interactive Lua state and the browser for up to the entrypoint timeout,
// and a user flipping through sources would find the plugin busy. Nothing is
// kept in Go once the flight ends: the plugin's cache is the one that knows
// when a token expires.

// resolveFlightTimeout caps a detached resolve. The Lua entrypoint has its
// own, shorter budget; this one only guards a backend that never returns.
var resolveFlightTimeout = 3 * time.Minute

// resolveRun performs the resolve for a flight; onProgress fans out to every
// caller.
type resolveRun func(ctx context.Context, onProgress engine.ProgressFunc) (*ResolvedStream, error)

type progressEvent struct{ status, message string }

type resolveFlight struct {
	done    chan struct{}
	ctx     context.Context // the resolve's own, detached from every caller
	cancel  context.CancelFunc
	warm    bool // started by a WarmStream
	callers int  // callers that started or joined it (under the group's mu)
	waiting int  // callers still waiting on it (under the group's mu)
	rs      *ResolvedStream
	err     error

	mu      sync.Mutex
	subs    map[uint64]engine.ProgressFunc
	nextSub uint64
	last    *progressEvent // replayed to late joiners
}

// subscribe adds fn to the progress fan-out, replaying the last event first.
// Call with f.mu held.
func (f *resolveFlight) subscribeLocked(fn engine.ProgressFunc) (unsub func()) {
	if fn == nil {
		return func() {}
	}
	id := f.nextSub
	f.nextSub++
	f.subs[id] = fn
	if f.last != nil {
		fn(f.last.status, f.last.message)
	}
	return func() {
		f.mu.Lock()
		delete(f.subs, id)
		f.mu.Unlock()
	}
}

// publish forwards a progress event to every subscriber. Under f.mu, so a
// joiner never sees the replay and a newer event out of order.
func (f *resolveFlight) publish(status, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = &progressEvent{status, message}
	for _, fn := range f.subs {
		fn(status, message)
	}
}

type resolveFlightGroup struct {
	mu sync.Mutex
	m  map[string]*resolveFlight
}

var resolveFlights = &resolveFlightGroup{m: map[string]*resolveFlight{}}

func resolveFlightKey(pluginID, streamID string) string { return pluginID + "\x00" + streamID }

// inFlight reports whether a resolve for key is running.
func (g *resolveFlightGroup) inFlight(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.m[key]
	return ok
}

// callers is how many callers started or joined key's flight (tests).
func (g *resolveFlightGroup) callers(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, ok := g.m[key]; ok {
		return f.callers
	}
	return 0
}

// size is the number of flights running (tests).
func (g *resolveFlightGroup) size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m)
}

// do starts the resolve for key, or joins the one running, and waits for it
// or for ctx. joined reports that another caller had started it, leaderWarm
// that the one running was a warm. The started resolve keeps ctx's values
// (profile, device) but not its cancellation.
func (g *resolveFlightGroup) do(ctx context.Context, key string, warm bool, onProgress engine.ProgressFunc, run resolveRun) (rs *ResolvedStream, joined, leaderWarm bool, err error) {
	g.mu.Lock()
	f, joined := g.m[key]
	if !joined {
		f = &resolveFlight{done: make(chan struct{}), warm: warm, subs: map[uint64]engine.ProgressFunc{}}
		f.ctx, f.cancel = context.WithTimeout(context.WithoutCancel(ctx), resolveFlightTimeout)
		g.m[key] = f
	}
	f.callers++
	f.waiting++
	// Subscribed before the leader's goroutine starts, so it misses no event.
	f.mu.Lock()
	unsub := f.subscribeLocked(onProgress)
	f.mu.Unlock()
	g.mu.Unlock()
	defer unsub()

	if !joined {
		go g.run(key, f, run)
	}
	select {
	case <-f.done:
		g.leave(key, f, false)
		return f.rs, joined, f.warm, f.err
	case <-ctx.Done():
		g.leave(key, f, true)
		return nil, joined, f.warm, ctx.Err()
	}
}

// leave drops a caller from f. When the last one gave up waiting on a flight
// a Play started, the resolve is cancelled and the flight leaves the map at
// once, so the next caller starts afresh instead of joining a dying one.
func (g *resolveFlightGroup) leave(key string, f *resolveFlight, gaveUp bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f.waiting--
	if !gaveUp || f.waiting > 0 || f.warm {
		return
	}
	if g.m[key] == f {
		delete(g.m, key)
	}
	f.cancel()
}

func (g *resolveFlightGroup) run(key string, f *resolveFlight, run resolveRun) {
	defer f.cancel()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[pileus/resolve] PANIC nel resolve condiviso: %v", r)
			f.rs, f.err = nil, fmt.Errorf("resolve: panic: %v", r)
		}
		g.mu.Lock()
		if g.m[key] == f {
			delete(g.m, key)
		}
		g.mu.Unlock()
		close(f.done)
	}()
	f.rs, f.err = run(f.ctx, f.publish)
}

// resolveShared resolves streamID through the flight group. A Play
// (warm=false) that joined a warm runs once more after it: on success that
// is a hit on the cache the warm just filled, and it does what a Play does
// that a warm must not (per-profile writes such as the remembered
// language); on failure it is the Play's own attempt. joined reports that
// the caller found a resolve already running, joinedWarm that it was a
// warm's.
func resolveShared(ctx context.Context, pluginID, streamID string, warm bool, onProgress engine.ProgressFunc, run resolveRun) (rs *ResolvedStream, joined, joinedWarm bool, err error) {
	key := resolveFlightKey(pluginID, streamID)
	rs, joined, leaderWarm, err := resolveFlights.do(ctx, key, warm, onProgress, run)
	joinedWarm = joined && leaderWarm
	if !warm && joinedWarm && ctx.Err() == nil {
		rs, _, _, err = resolveFlights.do(ctx, key, false, onProgress, run)
	}
	return rs, joined, joinedWarm, err
}
