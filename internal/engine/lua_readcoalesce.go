package engine

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// Read coalescing for the request/response entrypoints a user triggers by
// tapping (details, browse, search, filters, streams).
//
// Why: the client gives up after a timeout and the plugin call keeps running
// (a Lua call can't be cancelled), so a second tap on the same title used to
// start a SECOND identical call on another LState — with a pool of two, the
// retry of a slow call is what shuts the plugin down. Now identical calls
// made while one is running share its result, and a successful result is
// reused for a short while, so the retry that arrives just after the first
// finished is instant.
//
// The flight is independent of any caller's context: the call runs on the
// entrypoint's own budget (see callEntrypointJSON), so a caller that hangs up
// does not cancel what the others are waiting for.

const (
	readCacheTTL = 30 * time.Second
	readCacheMax = 256
)

type readFlight struct {
	done chan struct{}
	val  json.RawMessage
	err  error
}

type readEntry struct {
	val json.RawMessage
	exp time.Time
}

type readCoalescer struct {
	mu      sync.Mutex
	flights map[string]*readFlight
	cache   map[string]readEntry
}

// coalescibleRead reports whether ep is a read whose identical concurrent
// calls may be merged. resolve (per-attempt state, progress stream) and the
// catalogs (already cached by the gRPC handler) are deliberately left out.
func coalescibleRead(ep string) bool { return isReadEntrypoint(ep) }

func readKey(pluginID, ep, profileID string, args map[string]any) (string, bool) {
	// encoding/json sorts map keys, so equal args always give the same bytes.
	b, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	return pluginID + "\x00" + ep + "\x00" + profileID + "\x00" + string(b), true
}

func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	return append(json.RawMessage(nil), b...)
}

// do runs fn once per key at a time and shares its outcome.
func (c *readCoalescer) do(key string, fn func() (json.RawMessage, error)) (val json.RawMessage, err error) {
	c.mu.Lock()
	if c.flights == nil {
		c.flights = map[string]*readFlight{}
		c.cache = map[string]readEntry{}
	}
	if e, ok := c.cache[key]; ok {
		if time.Now().Before(e.exp) {
			c.mu.Unlock()
			return cloneRaw(e.val), nil
		}
		delete(c.cache, key)
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		<-f.done
		return cloneRaw(f.val), f.err
	}
	f := &readFlight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()

	// However fn ends — including a panic — the flight must be finished and
	// removed, or every later identical call would wait on it forever.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[lua] read coalescer: PANIC recuperato: %v", r)
			f.val, f.err = nil, fmt.Errorf("panic: %v", r)
			// The leader's own return values (a recovered panic returns the
			// zero values, i.e. a silent empty success).
			val, err = nil, f.err
		}
		c.mu.Lock()
		delete(c.flights, key)
		if f.err == nil && len(f.val) > 0 && string(f.val) != "null" {
			c.storeLocked(key, f.val)
		}
		c.mu.Unlock()
		close(f.done)
	}()
	f.val, f.err = fn()
	return cloneRaw(f.val), f.err
}

// storeLocked caches a successful, non-empty result. "null" means "not
// found" and may well exist a moment later, so it is never kept.
func (c *readCoalescer) storeLocked(key string, val json.RawMessage) {
	if len(c.cache) >= readCacheMax {
		now := time.Now()
		for k, e := range c.cache {
			if now.After(e.exp) {
				delete(c.cache, k)
			}
		}
		for k := range c.cache { // still full: make room, any entry will do
			if len(c.cache) < readCacheMax {
				break
			}
			delete(c.cache, k)
		}
	}
	c.cache[key] = readEntry{val: cloneRaw(val), exp: time.Now().Add(readCacheTTL)}
}
