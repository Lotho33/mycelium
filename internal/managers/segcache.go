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
// Short-lived in-memory store of upstream HLS bytes (media playlists and the
// first segments) filled by the pre-buffer step during ResolveStream, so the
// player's first /proxy requests are answered from RAM. Keyed by the
// absolute upstream URL (what the proxy decodes from ?data=). Entries expire
// fast and the store is byte-bounded with LRU eviction.

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
	maxBytes: 96 << 20, // ceiling across every stream
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

// Delete drops the entry for url. The playlist proxy serves a warm live
// playlist once and drops it: its segment window slides, so later reloads
// must come from upstream.
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
// Pre-buffer hook, implemented in internal/api (which imports pileus, so
// pileus can't import it) and injected here from that package's init().
// ─────────────────────────────────────────────────────────────────────────────

// PrefetchOptions configures a single PrefetchHLSHead run.
type PrefetchOptions struct {
	// Headers are the upstream headers the plugin's resolve returned, sent as
	// the player's proxied request would send them.
	Headers map[string]string
	// UseVPN routes the pre-fetch through the plugin's video egress.
	UseVPN bool
	// Egress is the name of that egress profile ("" = the single VPN proxy).
	Egress string
	// IsLive only affects logging here.
	IsLive bool
	// MaxSegments is the pre-fetch target. 0 disables pre-buffering entirely.
	MaxSegments int
	// MaxBytes stops the pre-fetch early once this many segment bytes are
	// buffered, regardless of MaxSegments. 0 = no byte limit.
	MaxBytes int64
	// MaxWait bounds the whole run; on expiry the partial result is returned
	// (TimedOut=true).
	MaxWait time.Duration
	// StartSec is where playback will start (a VOD resume point): warm the
	// segment containing it and the following ones. 0 = from the beginning.
	StartSec float64
}

// PrefetchProgress is one progress tick from PrefetchHLSHead.
type PrefetchProgress struct {
	// Phase: "playlist", then "segment", then "done" exactly once.
	Phase string
	Done  int   // segments buffered so far
	Total int   // segments targeted
	Bytes int64 // segment bytes buffered so far
}

// PrefetchResult is the outcome of a PrefetchHLSHead run.
type PrefetchResult struct {
	Segments int
	Bytes    int64
	TimedOut bool
	// Err is set only when the pre-buffer couldn't run at all; it never fails
	// the resolve.
	Err error
}

// PrefetchHLSHead pre-fetches the media playlist and the first
// opts.MaxSegments segments of rawURL into Segments, calling emit on
// progress. It honours ctx and MaxWait and never fails the resolve. Set by
// internal/api's init().
var PrefetchHLSHead func(ctx context.Context, rawURL string, opts PrefetchOptions, emit func(PrefetchProgress)) PrefetchResult
