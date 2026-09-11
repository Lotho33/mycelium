package managers

import (
	"sync"
	"time"

	"mycelium/internal/core"
)

const defaultCacheMaxItems = 1000

type cacheItem struct {
	value     any
	expiresAt int64
}

type CacheManager struct {
	items    map[string]cacheItem
	mu       sync.RWMutex
	maxItems int
}

var Cache = &CacheManager{
	items:    make(map[string]cacheItem),
	maxItems: defaultCacheMaxItems,
}

var cacheGCStop = make(chan struct{})

func init() {
	go func() {
		t := time.NewTicker(1 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				func() {
					defer core.Guard("managers/cache-gc-tick")
					Cache.cleanup()
				}()
			case <-cacheGCStop:
				return
			}
		}
	}()
}

// SetMaxItems configures the capacity cap. Call before serving traffic.
func (c *CacheManager) SetMaxItems(n int) {
	if n > 0 {
		c.mu.Lock()
		c.maxItems = n
		c.mu.Unlock()
	}
}

// Get returns a value. Returns (nil, false) when missing or expired.
func (c *CacheManager) Get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	item, found := c.items[key]
	if !found {
		return nil, false
	}
	if time.Now().UnixNano() > item.expiresAt {
		return nil, false
	}
	return item.value, true
}

// Set stores a value with a TTL in seconds.
// When at capacity, the entry with the earliest expiry is evicted first.
func (c *CacheManager) Set(key string, value any, expireSeconds int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.items[key]; !exists && len(c.items) >= c.maxItems {
		c.evictOldest()
	}
	c.items[key] = cacheItem{
		value:     value,
		expiresAt: time.Now().Add(time.Duration(expireSeconds) * time.Second).UnixNano(),
	}
}

// Delete removes a key.
func (c *CacheManager) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// Flush empties the cache.
func (c *CacheManager) Flush() {
	c.mu.Lock()
	c.items = make(map[string]cacheItem)
	c.mu.Unlock()
}

// cleanup removes all expired entries (called by the background ticker).
func (c *CacheManager) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UnixNano()
	for k, v := range c.items {
		if now > v.expiresAt {
			delete(c.items, k)
		}
	}
}

// evictOldest removes the entry with the earliest expiry to make room.
// Must be called with c.mu held for writing.
func (c *CacheManager) evictOldest() {
	var (
		oldestKey string
		oldestExp int64
		first     = true
	)
	for k, v := range c.items {
		if first || v.expiresAt < oldestExp {
			oldestKey = k
			oldestExp = v.expiresAt
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.items, oldestKey)
	}
}
