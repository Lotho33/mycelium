package engine

import (
	"context"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// mycelium.ratelimit: named token buckets shared by every state of a plugin,
// for the external services a plugin must not hammer (metadata APIs, a
// chat bot). set() configures a bucket, wait() takes a token (sleeping
// if needed), pause() stops the bucket for a while after the service said
// "slow down" (HTTP 429, FLOOD_WAIT).

// maxRatelimitWait caps how long one wait() may sleep.
const maxRatelimitWait = 60 * time.Second

type tokenBucket struct {
	mu          sync.Mutex
	rate        float64 // tokens per second
	burst       float64
	tokens      float64
	last        time.Time
	pausedUntil time.Time
}

// reserve takes a token and returns how long the caller must sleep before
// using it. If that's longer than maxWait it takes nothing and returns
// ok=false.
func (b *tokenBucket) reserve(now time.Time, maxWait time.Duration) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	var wait time.Duration
	if b.tokens < 1 {
		wait = time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
	}
	if p := b.pausedUntil.Sub(now); p > wait {
		wait = p
	}
	if wait > maxWait {
		return wait, false
	}
	b.tokens-- // may go negative: later callers queue behind this one
	return wait, true
}

type limiterStore struct {
	mu sync.Mutex
	m  map[string]*tokenBucket // plugin id + "\x00" + name
}

var pluginLimiters = &limiterStore{m: map[string]*tokenBucket{}}

func (s *limiterStore) bucket(plugin, name string) *tokenBucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[plugin+"\x00"+name]
}

// set creates or reconfigures a bucket; an existing bucket keeps its tokens
// and pause.
func (s *limiterStore) set(plugin, name string, perSecond, burst float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := plugin + "\x00" + name
	b := s.m[k]
	if b == nil {
		s.m[k] = &tokenBucket{rate: perSecond, burst: burst, tokens: burst}
		return
	}
	b.mu.Lock()
	b.rate, b.burst = perSecond, burst
	b.tokens = min(b.tokens, burst)
	b.mu.Unlock()
}

// wait takes a token from the bucket, sleeping as needed (at most maxWait,
// and never past ctx). An unknown bucket doesn't limit. false = gave up.
func (s *limiterStore) wait(ctx context.Context, plugin, name string, maxWait time.Duration) bool {
	b := s.bucket(plugin, name)
	if b == nil {
		return true
	}
	maxWait = min(maxWait, maxRatelimitWait)
	if dl, ok := ctx.Deadline(); ok {
		maxWait = min(maxWait, time.Until(dl))
	}
	d, ok := b.reserve(time.Now(), maxWait)
	if !ok {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *limiterStore) pause(plugin, name string, d time.Duration) {
	b := s.bucket(plugin, name)
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if until := time.Now().Add(d); until.After(b.pausedUntil) {
		b.pausedUntil = until
	}
}

func (s *limiterStore) dropPlugin(plugin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.m {
		if len(k) > len(plugin) && k[:len(plugin)] == plugin && k[len(plugin)] == 0 {
			delete(s.m, k)
		}
	}
}

func buildRatelimitModule(L *lua.LState, opts SDKOpts, scope *callScope) *lua.LTable {
	t := L.NewTable()
	plugin := opts.PluginID

	// mycelium.ratelimit.set(name, per_second [, burst]) — burst defaults to
	// 1 (strictly spaced calls).
	t.RawSetString("set", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		rate := float64(L.CheckNumber(2))
		burst := float64(L.OptNumber(3, 1))
		if rate <= 0 || burst < 1 {
			L.ArgError(2, "per_second must be > 0 and burst >= 1")
		}
		pluginLimiters.set(plugin, name, rate, burst)
		return 0
	}))

	// mycelium.ratelimit.wait(name [, max_wait_seconds]) → bool — false when
	// the wait would exceed max_wait (default 30, max 60) or the call ended.
	t.RawSetString("wait", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		maxWait := time.Duration(float64(L.OptNumber(2, 30)) * float64(time.Second))
		L.Push(lua.LBool(pluginLimiters.wait(scope.parentCtx(), plugin, name, maxWait)))
		return 1
	}))

	// mycelium.ratelimit.pause(name, seconds) — no token until then (e.g.
	// the Retry-After of a 429).
	t.RawSetString("pause", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		sec := float64(L.CheckNumber(2))
		if sec > 0 {
			pluginLimiters.pause(plugin, name, time.Duration(min(sec, 3600)*float64(time.Second)))
		}
		return 0
	}))

	return t
}
