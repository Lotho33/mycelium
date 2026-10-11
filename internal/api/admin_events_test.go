package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdminEventsSignalsChangedSection(t *testing.T) {
	var state atomic.Int32
	origTopics, origTick := adminTopics, adminEventsTick
	adminTopics = map[string]http.HandlerFunc{
		"things": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte{byte('0' + state.Load())})
		},
		"broken": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusInternalServerError)
		},
	}
	adminEventsTick = 20 * time.Millisecond
	t.Cleanup(func() { adminTopics, adminEventsTick = origTopics, origTick })

	srv := httptest.NewServer(http.HandlerFunc(getAdminEvents))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	next := func() string {
		for lines.Scan() {
			if l := lines.Text(); strings.HasPrefix(l, "data: ") {
				return strings.TrimPrefix(l, "data: ")
			}
		}
		t.Fatalf("stream ended: %v", lines.Err())
		return ""
	}

	if got := next(); got != `{"topic":"*"}` {
		t.Fatalf("first message %s, want the resync", got)
	}
	time.Sleep(100 * time.Millisecond) // baseline taken, nothing changed
	state.Store(1)
	if got := next(); got != `{"topic":"things"}` {
		t.Fatalf("got %s, want the changed section", got)
	}
	// A poke (admin write) is picked up without waiting for a slow tick.
	adminEventsTick = time.Hour
	state.Store(2)
	adminEvents.Poke()
	if got := next(); got != `{"topic":"things"}` {
		t.Fatalf("after poke got %s", got)
	}
}
