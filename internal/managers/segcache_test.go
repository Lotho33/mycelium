package managers

import (
	"container/list"
	"testing"
	"time"
)

func newTestSegCache(maxBytes int, ttl time.Duration) *segCache {
	return &segCache{
		items:    map[string]*segCacheEntry{},
		lru:      list.New(),
		maxBytes: maxBytes,
		ttl:      ttl,
	}
}

func TestSegCachePutGet(t *testing.T) {
	c := newTestSegCache(1<<20, time.Minute)
	c.Put("u1", []byte("hello"))

	got, ok := c.Get("u1")
	if !ok || string(got) != "hello" {
		t.Fatalf("Get(u1) = %q,%v; want \"hello\",true", got, ok)
	}
	// Returned slice must be a copy — mutating it must not corrupt the entry.
	got[0] = 'H'
	again, _ := c.Get("u1")
	if string(again) != "hello" {
		t.Fatalf("cache entry mutated through returned slice: %q", again)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatalf("Get(missing) reported a hit")
	}
}

func TestSegCacheTTLExpiry(t *testing.T) {
	c := newTestSegCache(1<<20, 20*time.Millisecond)
	c.Put("u1", []byte("data"))
	if _, ok := c.Get("u1"); !ok {
		t.Fatalf("entry gone before TTL")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.Get("u1"); ok {
		t.Fatalf("entry still present after TTL")
	}
	if c.Len() != 0 {
		t.Fatalf("expired entry not pruned on Get: Len=%d", c.Len())
	}
}

func TestSegCacheByteCeilingEvictsLRU(t *testing.T) {
	c := newTestSegCache(30, time.Minute) // room for ~2 x 10-byte bodies
	c.Put("a", make([]byte, 10))
	c.Put("b", make([]byte, 10))
	_, _ = c.Get("a") // touch a → b is now LRU
	c.Put("c", make([]byte, 10))
	c.Put("d", make([]byte, 10)) // forces eviction below/at ceiling

	if c.ByteLen() > 30 {
		t.Fatalf("cache over ceiling: %d bytes", c.ByteLen())
	}
	if _, ok := c.Get("b"); ok {
		t.Fatalf("LRU entry b survived eviction")
	}
	if _, ok := c.Get("d"); !ok {
		t.Fatalf("most-recent entry d was evicted")
	}
}

func TestSegCacheOversizedBodyRejected(t *testing.T) {
	c := newTestSegCache(8, time.Minute)
	c.Put("big", make([]byte, 100))
	if _, ok := c.Get("big"); ok {
		t.Fatalf("body larger than the whole cache was stored")
	}
	if c.ByteLen() != 0 {
		t.Fatalf("byte counter drifted: %d", c.ByteLen())
	}
}

func TestSegCacheReplaceKeepsAccounting(t *testing.T) {
	c := newTestSegCache(1<<20, time.Minute)
	c.Put("u1", make([]byte, 10))
	c.Put("u1", make([]byte, 3))
	if c.Len() != 1 {
		t.Fatalf("replace created a second entry: Len=%d", c.Len())
	}
	if c.ByteLen() != 3 {
		t.Fatalf("byte counter after replace = %d; want 3", c.ByteLen())
	}
}
