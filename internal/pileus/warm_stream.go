package pileus

import (
	"context"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// WarmStream: resolve a stream ahead of Play (the client calls it when a
// details page or an episode opens), so the plugin's resolve cache is warm
// when ResolveStream comes and Play only waits for the pre-buffer. No
// playback lease, no session, no proxy URL, no segments. Traffic towards the
// sites stays low: one warm at a time server-wide, never while a Play is
// resolving or the browser is busy, not twice for the same stream within
// warmRecentTTL, and at most warm_resolve_per_hour per plugin.

// WarmStream outcomes and skip reasons (WarmStreamResponse).
const (
	warmStarted = "started"
	warmJoined  = "joined"
	warmSkipped = "skipped"

	warmReasonDisabled    = "disabled"
	warmReasonBusy        = "busy"
	warmReasonBudget      = "budget"
	warmReasonBrowserBusy = "browser_busy"
	warmReasonRecent      = "recent"
)

const (
	// warmRecentTTL: a stream warmed (or played) this recently is not warmed
	// again. It doesn't prove the plugin's cache is still warm, it bounds calls.
	warmRecentTTL = 30 * time.Minute
	// warmHitWindow: a Play within this long of a completed warm of the same
	// stream counts as a warm hit.
	warmHitWindow = 15 * time.Minute
	// warmDefaultPerHour is the per-plugin budget when warm_resolve_per_hour is
	// unset.
	warmDefaultPerHour = 20
	// maxWarmStreamIDLen bounds the stream id a client may send.
	maxWarmStreamIDLen = 1024
)

// resolvesInFlight counts ResolveStream calls still resolving: no warm
// starts while a real Play waits on a plugin.
var resolvesInFlight atomic.Int64

// Seams for tests.
var (
	// warmEnabled: the global switch and the plugin's own (manifest +
	// dashboard setting).
	warmEnabled = func(pluginID string) bool {
		return managers.Settings.GetBool("warm_resolve_enabled", true) && engine.LuaPlugins.WarmResolveEnabled(pluginID)
	}
	warmBrowserBusy = func() bool { return engine.BrowserCallsInFlight() > 0 }
	warmRunFor      = func(pluginID, streamID string) resolveRun {
		p := &luaPipelineProvider{id: pluginID}
		return func(ctx context.Context, onProgress engine.ProgressFunc) (*ResolvedStream, error) {
			return p.warmResolve(ctx, streamID, onProgress)
		}
	}
	warmPerHour = func() float64 {
		v := strings.TrimSpace(managers.Settings.GetString("warm_resolve_per_hour", ""))
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 {
			return n
		}
		return warmDefaultPerHour
	}
)

type warmBucket struct {
	tokens float64
	last   time.Time
}

// warmState is the bookkeeping behind the WarmStream checks.
type warmState struct {
	sem chan struct{} // one warm at a time, server-wide

	mu        sync.Mutex
	recent    map[string]time.Time // key → last warm start or Play resolve
	completed map[string]time.Time // key → last successful warm, for warm_hit
	buckets   map[string]*warmBucket
}

func newWarmState() *warmState {
	return &warmState{
		sem:       make(chan struct{}, 1),
		recent:    map[string]time.Time{},
		completed: map[string]time.Time{},
		buckets:   map[string]*warmBucket{},
	}
}

var warmer = newWarmState()

func (w *warmState) tryAcquire() (release func(), ok bool) {
	select {
	case w.sem <- struct{}{}:
		return func() { <-w.sem }, true
	default:
		return nil, false
	}
}

// pruneLocked drops entries past their window (the maps stay small).
func (w *warmState) pruneLocked(now time.Time) {
	for k, t := range w.recent {
		if now.Sub(t) > warmRecentTTL {
			delete(w.recent, k)
		}
	}
	for k, t := range w.completed {
		if now.Sub(t) > warmHitWindow {
			delete(w.completed, k)
		}
	}
}

func (w *warmState) isRecent(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	w.pruneLocked(now)
	t, ok := w.recent[key]
	return ok && now.Sub(t) <= warmRecentTTL
}

func (w *warmState) markRecent(key string) {
	w.mu.Lock()
	w.recent[key] = time.Now()
	w.mu.Unlock()
}

func (w *warmState) forgetRecent(key string) {
	w.mu.Lock()
	delete(w.recent, key)
	w.mu.Unlock()
}

func (w *warmState) markCompleted(key string) {
	w.mu.Lock()
	w.completed[key] = time.Now()
	w.mu.Unlock()
}

// takeHit reports (once) whether key was warmed within warmHitWindow.
func (w *warmState) takeHit(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.completed[key]
	if !ok {
		return false
	}
	delete(w.completed, key)
	return time.Since(t) <= warmHitWindow
}

// takeBudget spends one token of pluginID's hourly bucket.
func (w *warmState) takeBudget(pluginID string, perHour float64) bool {
	if perHour <= 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	b := w.buckets[pluginID]
	if b == nil {
		b = &warmBucket{tokens: perHour, last: now}
		w.buckets[pluginID] = b
	}
	b.tokens = min(perHour, b.tokens+now.Sub(b.last).Hours()*perHour)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (h *MediaHandler) WarmStream(ctx context.Context, req *gen.WarmStreamRequest) (*gen.WarmStreamResponse, error) {
	if req.PluginId == "" || req.StreamId == "" || len(req.StreamId) > maxWarmStreamIDLen {
		return nil, status.Error(codes.InvalidArgument, "plugin_id e stream_id obbligatori")
	}
	pluginID, streamID := req.PluginId, req.StreamId
	key := resolveFlightKey(pluginID, streamID)
	// Skips are only counted (dashboard), not logged: a client calling in a
	// loop would flood the log.
	skip := func(reason string) (*gen.WarmStreamResponse, error) {
		warmStats.skip(reason)
		return &gen.WarmStreamResponse{Outcome: warmSkipped, Reason: reason}, nil
	}

	// Cheapest checks first.
	if !warmEnabled(pluginID) {
		return skip(warmReasonDisabled)
	}
	if resolveFlights.inFlight(key) {
		warmStats.joined.Add(1)
		return &gen.WarmStreamResponse{Outcome: warmJoined}, nil
	}
	if warmer.isRecent(key) {
		return skip(warmReasonRecent)
	}
	release, ok := warmer.tryAcquire()
	if !ok {
		return skip(warmReasonBusy)
	}
	if resolvesInFlight.Load() > 0 {
		release()
		return skip(warmReasonBusy)
	}
	if warmBrowserBusy() {
		release()
		return skip(warmReasonBrowserBusy)
	}
	if !warmer.takeBudget(pluginID, warmPerHour()) {
		release()
		return skip(warmReasonBudget)
	}

	warmer.markRecent(key)
	warmStats.started.Add(1)
	log.Printf("[pileus/warm] %s %s started", pluginID, streamID)
	base := warmRunFor(pluginID, streamID)
	// Marked inside the flight, before it reports done: a Play that joined
	// this warm then finds it completed (warm_hit) when its turn comes.
	run := func(rctx context.Context, onProgress engine.ProgressFunc) (*ResolvedStream, error) {
		rs, err := base(rctx, onProgress)
		if err == nil {
			warmer.markCompleted(key)
		}
		return rs, err
	}
	// Keeps the caller's profile and device, not its cancellation: the RPC
	// returns now.
	wctx := context.WithoutCancel(ctx)
	go func() {
		defer release()
		defer core.Guard("pileus/warm")
		start := time.Now()
		_, joinedPlay, _, err := resolveShared(wctx, pluginID, streamID, true, nil, run)
		d := time.Since(start)
		warmStats.warmDur.add(d)
		via := ""
		if joinedPlay {
			via = " (agganciato a un Play)"
		}
		if err != nil {
			warmStats.completedErr.Add(1)
			if errors.Is(err, engine.ErrPluginBusy) {
				// No Lua state free: nothing reached the site, so it may be tried
				// again before warmRecentTTL.
				warmer.forgetRecent(key)
			}
			log.Printf("[pileus/warm] %s %s failed %.1fs%s: %v", pluginID, streamID, d.Seconds(), via, err)
			return
		}
		warmStats.completedOK.Add(1)
		log.Printf("[pileus/warm] %s %s done %.1fs%s", pluginID, streamID, d.Seconds(), via)
	}()
	return &gen.WarmStreamResponse{Outcome: warmStarted}, nil
}

// ── metrics ──────────────────────────────────────────────────────────────────

// durRing keeps the last durations for mean and p90.
type durRing struct {
	mu  sync.Mutex
	buf []time.Duration
	pos int
}

const durRingSize = 200

func (r *durRing) add(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) < durRingSize {
		r.buf = append(r.buf, d)
		return
	}
	r.buf[r.pos] = d
	r.pos = (r.pos + 1) % durRingSize
}

// stats returns count, mean and p90 in milliseconds.
func (r *durRing) stats() (n int, meanMs, p90Ms int64) {
	r.mu.Lock()
	s := slices.Clone(r.buf)
	r.mu.Unlock()
	if len(s) == 0 {
		return 0, 0, 0
	}
	slices.Sort(s)
	var sum time.Duration
	for _, d := range s {
		sum += d
	}
	return len(s), (sum / time.Duration(len(s))).Milliseconds(), s[(len(s)*9-1)/10].Milliseconds()
}

type warmMetrics struct {
	started, joined           atomic.Int64
	completedOK, completedErr atomic.Int64
	hits                      atomic.Int64 // Plays served by a completed warm
	hitsInflight              atomic.Int64 // of which joined the warm still running

	mu      sync.Mutex
	skipped map[string]int64

	warmDur       durRing
	resolveFresh  durRing // ResolveStream that ran its own resolve
	resolveJoined durRing // ResolveStream that joined one in flight
}

var warmStats = &warmMetrics{skipped: map[string]int64{}}

func (m *warmMetrics) skip(reason string) {
	m.mu.Lock()
	m.skipped[reason]++
	m.mu.Unlock()
}

// recordResolve notes a ResolveStream's resolve phase.
func (m *warmMetrics) recordResolve(d time.Duration, joined bool) {
	if joined {
		m.resolveJoined.add(d)
	} else {
		m.resolveFresh.add(d)
	}
}

// resolveCacheHitUnder: a fresh resolve this fast was served from the
// plugin's cache.
const resolveCacheHitUnder = 300 * time.Millisecond

// WarmStatsSnapshot is the dashboard's view of the warm counters.
func WarmStatsSnapshot() map[string]any {
	m := warmStats
	m.mu.Lock()
	skipped := make(map[string]int64, len(m.skipped))
	for k, v := range m.skipped {
		skipped[k] = v
	}
	m.mu.Unlock()
	durMap := func(r *durRing) map[string]int64 {
		n, mean, p90 := r.stats()
		return map[string]int64{"count": int64(n), "mean_ms": mean, "p90_ms": p90}
	}
	fresh := durMap(&m.resolveFresh)
	m.resolveFresh.mu.Lock()
	var cacheHits int64
	for _, d := range m.resolveFresh.buf {
		if d < resolveCacheHitUnder {
			cacheHits++
		}
	}
	m.resolveFresh.mu.Unlock()
	fresh["cache_hits"] = cacheHits
	return map[string]any{
		"warm_started":            m.started.Load(),
		"warm_joined":             m.joined.Load(),
		"warm_skipped":            skipped,
		"warm_completed_ok":       m.completedOK.Load(),
		"warm_completed_err":      m.completedErr.Load(),
		"warm_hit":                m.hits.Load(),
		"warm_hit_inflight":       m.hitsInflight.Load(),
		"warm_duration":           durMap(&m.warmDur),
		"resolve_duration_fresh":  fresh,
		"resolve_duration_joined": durMap(&m.resolveJoined),
	}
}
