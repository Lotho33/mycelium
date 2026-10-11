package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Live dashboard updates. GET /admin/events is an SSE stream of
// {"topic": "..."} messages, one per dashboard section whose data changed;
// the page then reloads just that section.
//
// Change detection is by snapshot, not by hooks: while at least one dashboard
// is connected, a watcher renders each section's own GET handler in memory
// and compares a hash with the previous one. Plugins, downloads, devices…
// change state from many places (lifecycle, cron tasks, crashes, the Pileus
// app), and rendering what the page itself would fetch catches all of them
// with no hook to forget. Any admin write pokes the watcher so the page
// reflects it at once, not at the next tick.

// adminTopics maps a topic to the handler whose output the page shows.
var adminTopics = map[string]http.HandlerFunc{
	"plugins":   adminPluginsSnapshot,
	"egress":    listEgress,
	"devices":   listPileusDevices,
	"profiles":  listPileusProfiles,
	"downloads": listDownloadsAdmin,
}

// adminPluginsSnapshot is what the plugin page renders: the plugins and the
// enricher bindings shown on their cards.
func adminPluginsSnapshot(w http.ResponseWriter, r *http.Request) {
	getPluginsInfo(w, r)
	getEnricherBindings(w, r)
}

// adminEventsTick is the watcher period. A var for tests.
var adminEventsTick = 1500 * time.Millisecond

type adminEventHub struct {
	mu     sync.Mutex
	subs   map[chan string]struct{}
	hashes map[string][32]byte
	poke   chan struct{}
	run    bool
}

var adminEvents = &adminEventHub{
	subs:   map[chan string]struct{}{},
	hashes: map[string][32]byte{},
	poke:   make(chan struct{}, 1),
}

// subscribe registers a dashboard and starts the watcher if it isn't running.
func (h *adminEventHub) subscribe() chan string {
	ch := make(chan string, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	if !h.run {
		h.run = true
		go h.watch()
	}
	h.mu.Unlock()
	return ch
}

func (h *adminEventHub) unsubscribe(ch chan string) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// Poke asks for an immediate check (after an admin write).
func (h *adminEventHub) Poke() {
	select {
	case h.poke <- struct{}{}:
	default:
	}
}

// watch runs while someone is subscribed; it exits (and forgets the hashes,
// so a later dashboard starts from a fresh baseline) when the last one leaves.
func (h *adminEventHub) watch() {
	t := time.NewTicker(adminEventsTick)
	defer t.Stop()
	for {
		h.mu.Lock()
		if len(h.subs) == 0 {
			h.run = false
			h.hashes = map[string][32]byte{}
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
		h.check()
		select {
		case <-t.C:
		case <-h.poke:
			// Writes often apply asynchronously (a plugin restart): look again
			// shortly after too.
			h.check()
			time.Sleep(400 * time.Millisecond)
		}
	}
}

// check renders every topic and broadcasts the ones that changed.
func (h *adminEventHub) check() {
	for topic, handler := range adminTopics {
		sum, ok := snapshotHash(handler)
		if !ok {
			continue
		}
		h.mu.Lock()
		prev, seen := h.hashes[topic]
		h.hashes[topic] = sum
		var targets []chan string
		if seen && prev != sum {
			for ch := range h.subs {
				targets = append(targets, ch)
			}
		}
		h.mu.Unlock()
		for _, ch := range targets {
			select {
			case ch <- topic:
			default: // a stalled page: it gets the next change
			}
		}
	}
}

// snapshotHash renders handler in memory; ok=false when it failed (no
// change is signalled for a section that can't be read right now).
func snapshotHash(handler http.HandlerFunc) (sum [32]byte, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/admin/events/snapshot", nil))
	if rec.Code != http.StatusOK {
		return sum, false
	}
	return sha256.Sum256(bytes.TrimSpace(rec.Body.Bytes())), true
}

// getAdminEvents is GET /admin/events.
func getAdminEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := adminEvents.subscribe()
	defer adminEvents.unsubscribe(ch)

	// Tells the page to resync once: it may have missed changes while
	// disconnected.
	fmt.Fprint(w, "retry: 3000\ndata: {\"topic\":\"*\"}\n\n")
	flusher.Flush()

	cookie, _ := r.Cookie(adminSessionCookie)
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case topic := <-ch:
			b, _ := json.Marshal(map[string]string{"topic": topic})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-heartbeat.C:
			// A logged-out or expired session stops receiving updates.
			if cookie == nil || !isValidAdminSession(cookie.Value) {
				return
			}
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
