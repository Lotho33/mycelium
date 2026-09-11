package managers

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Warm HLS head cache
// ─────────────────────────────────────────────────────────────────────────────
//
// Short-lived in-memory store of upstream HLS bytes — media playlists and the
// first few media segments — that the pre-buffer step fills during
// ResolveStream (see PrefetchHLSHead), so the player's very first requests to
// /proxy/playlist.m3u8 and /proxy/segment.ts are answered from RAM instead of
// a cold upstream round-trip. That is also what lets ResolveStream emit real
// "3/8 segmenti" progress for the buffering phase instead of a blind spinner.
//
// Keyed by the ABSOLUTE upstream URL — exactly the value ProxyPlaylist /
// ProxySegment decode from their ?data= query param, so a hand-off is a plain
// map lookup with no key derivation.
//
// Deliberately tiny and self-evicting: a hand-off buffer for the first ~10s of
// playback, not a CDN. Entries expire fast (TTL) and the whole store is
// byte-bounded with LRU eviction so a burst of starts can't grow it without
// bound.

type segCacheEntry struct {
	body      []byte
	expiresAt time.Time
	elem      *list.Element // node in lru; its Value is the map key (string)
}

type segCache struct {
	mu       sync.Mutex
	items    map[string]*segCacheEntry
	lru      *list.List // front = most-recently-used
	bytes    int
	maxBytes int
	ttl      time.Duration
}

// Segments is the process-wide warm cache.
var Segments = &segCache{
	items:    make(map[string]*segCacheEntry),
	lru:      list.New(),
	maxBytes: 96 << 20, // 96 MiB ceiling across every in-flight stream
	ttl:      90 * time.Second,
}

// SegmentCacheConfigure overrides the ceiling / TTL. A zero argument keeps the
// current value. Called once at settings load.
func SegmentCacheConfigure(maxBytes int, ttl time.Duration) {
	Segments.mu.Lock()
	defer Segments.mu.Unlock()
	if maxBytes > 0 {
		Segments.maxBytes = maxBytes
	}
	if ttl > 0 {
		Segments.ttl = ttl
	}
	Segments.evictLocked()
}

// Get returns a copy of the cached body for url, or (nil, false) on miss or
// expiry. A copy because callers stream it straight to an http.ResponseWriter
// or rewrite around it.
func (c *segCache) Get(url string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[url]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		c.removeLocked(url, e)
		return nil, false
	}
	c.lru.MoveToFront(e.elem)
	out := make([]byte, len(e.body))
	copy(out, e.body)
	return out, true
}

// Put stores body under url (replacing any existing entry) and evicts LRU
// entries until the byte ceiling is satisfied. A no-op for an empty body or a
// body larger than the whole cache.
func (c *segCache) Put(url string, body []byte) {
	if len(body) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items[url]; ok {
		c.removeLocked(url, old)
	}
	if len(body) > c.maxBytes {
		return
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	e := &segCacheEntry{body: cp, expiresAt: time.Now().Add(c.ttl)}
	e.elem = c.lru.PushFront(url)
	c.items[url] = e
	c.bytes += len(cp)
	c.evictLocked()
}

// Delete drops the entry for url if present. Used by the playlist proxy to
// evict a warm media playlist the moment it has served it once: a live
// playlist has a sliding segment window, so every reload after the first
// MUST hit upstream for fresh segments — serving the frozen resolve-time
// copy for the whole TTL makes the player run out of segments and stall a
// few seconds in ("stops transmitting after a while").
func (c *segCache) Delete(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[url]; ok {
		c.removeLocked(url, e)
	}
}

// Len / Bytes are for tests and diagnostics.
func (c *segCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *segCache) ByteLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// Reset drops everything — tests only.
func (c *segCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*segCacheEntry)
	c.lru.Init()
	c.bytes = 0
}

func (c *segCache) removeLocked(url string, e *segCacheEntry) {
	c.bytes -= len(e.body)
	c.lru.Remove(e.elem)
	delete(c.items, url)
}

func (c *segCache) evictLocked() {
	for c.bytes > c.maxBytes {
		back := c.lru.Back()
		if back == nil {
			return
		}
		url, _ := back.Value.(string)
		if e, ok := c.items[url]; ok {
			c.removeLocked(url, e)
		} else {
			c.lru.Remove(back)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Pre-buffer hook (implemented in internal/api, injected here)
// ─────────────────────────────────────────────────────────────────────────────
//
// internal/pileus can't import internal/api (that would be an import cycle:
// api already imports pileus), yet the HLS head pre-fetcher needs proxy.go's
// upstream HTTP plumbing (browser-baseline headers, uTLS transport, VPN
// client selection). So it lives in internal/api and is injected here as a
// func var during that package's init(). It stays nil in pileus unit tests
// unless a test sets a fake.

// PrefetchOptions configures a single PrefetchHLSHead run.
type PrefetchOptions struct {
	// Headers are the upstream headers the plugin's resolve returned
	// (Referer / Cookie / User-Agent / …), canonical-cased. Used verbatim so
	// the pre-fetch request is byte-for-byte what the player's own proxy call
	// would send — important for CDNs that 403 on a header mismatch.
	Headers map[string]string
	// UseVPN routes the pre-fetch through the same VPN egress the plugin's
	// video traffic uses.
	UseVPN bool
	// Egress is the name of the egress profile that video traffic uses (see
	// managers.EgressProfiles). Empty = the legacy single VPN proxy.
	Egress string
	// IsLive only affects logging/telemetry here; the segment target is
	// chosen by the caller and passed as MaxSegments.
	IsLive bool
	// MaxSegments is the pre-fetch target. 0 disables pre-buffering entirely.
	MaxSegments int
	// MaxBytes stops the pre-fetch early once this many segment bytes are
	// buffered, regardless of MaxSegments. 0 = no byte limit.
	MaxBytes int64
	// MaxWait is a hard ceiling on the whole run. On expiry the partial
	// result is returned (TimedOut=true) — the RPC never hangs on it.
	MaxWait time.Duration
}

// PrefetchProgress is one progress tick from PrefetchHLSHead.
type PrefetchProgress struct {
	// Phase: "playlist" while fetching the master/variant m3u8, "segment"
	// while pulling media segments, "done" exactly once as the final tick.
	Phase string
	Done  int   // segments buffered so far
	Total int   // segments targeted (min(MaxSegments, segments in playlist))
	Bytes int64 // segment bytes buffered so far
}

// PrefetchResult is the outcome of a PrefetchHLSHead run.
type PrefetchResult struct {
	Segments int
	Bytes    int64
	TimedOut bool
	// Err is set only when the pre-buffer could not run at all (bad playlist,
	// no upstream client). It is never a reason to fail the resolve — the
	// caller proceeds without a warm cache.
	Err error
}

// PrefetchHLSHead pre-fetches the media playlist + first opts.MaxSegments
// segments for rawURL into Segments, calling emit after each unit of
// progress. It honours ctx cancellation and its own MaxWait, and never
// returns an error that should abort the resolve.
//
// nil until internal/api's init() runs (always does in the server binary).
var PrefetchHLSHead func(ctx context.Context, rawURL string, opts PrefetchOptions, emit func(PrefetchProgress)) PrefetchResult
