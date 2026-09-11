package pileus

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"mycelium/internal/managers"
)

type progMsg struct{ status, message string }

// captureProgress returns a sendProgress func plus a getter for what it saw.
func captureProgress() (func(string, string) error, func() []progMsg) {
	var mu sync.Mutex
	var msgs []progMsg
	send := func(status, message string) error {
		mu.Lock()
		msgs = append(msgs, progMsg{status, message})
		mu.Unlock()
		return nil
	}
	get := func() []progMsg {
		mu.Lock()
		defer mu.Unlock()
		out := make([]progMsg, len(msgs))
		copy(out, msgs)
		return out
	}
	return send, get
}

func swapHook(t *testing.T, fn func(context.Context, string, managers.PrefetchOptions, func(managers.PrefetchProgress)) managers.PrefetchResult) {
	t.Helper()
	prev := managers.PrefetchHLSHead
	managers.PrefetchHLSHead = fn
	t.Cleanup(func() { managers.PrefetchHLSHead = prev })
}

func TestPrebufferHLS_EmitsLoadingProgressVerbatim(t *testing.T) {
	swapHook(t, func(_ context.Context, _ string, opts managers.PrefetchOptions, emit func(managers.PrefetchProgress)) managers.PrefetchResult {
		if opts.MaxSegments != 3 {
			t.Errorf("MaxSegments = %d; want 3 (vod default)", opts.MaxSegments)
		}
		emit(managers.PrefetchProgress{Phase: "playlist"})
		emit(managers.PrefetchProgress{Phase: "segment", Done: 1, Total: 3})
		emit(managers.PrefetchProgress{Phase: "segment", Done: 2, Total: 3})
		emit(managers.PrefetchProgress{Phase: "segment", Done: 3, Total: 3})
		emit(managers.PrefetchProgress{Phase: "done", Done: 3, Total: 3})
		return managers.PrefetchResult{Segments: 3}
	})

	send, get := captureProgress()
	prebufferHLS(context.Background(), "example.movie", "http://cdn/x.m3u8", map[string]string{}, false, send)

	msgs := get()
	if len(msgs) != 4 {
		t.Fatalf("want 4 progress events (playlist + 3 segment, done suppressed); got %d: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.status != "loading" {
			t.Errorf("status = %q; must stay \"loading\" for backward compat (%+v)", m.status, m)
		}
		if !strings.HasPrefix(m.message, "Preparazione flusso…") {
			t.Errorf("message not human-readable prefix: %q", m.message)
		}
	}
	if msgs[2].message != "Preparazione flusso… 2/3 segmenti" {
		t.Errorf("segment message = %q; want \"Preparazione flusso… 2/3 segmenti\"", msgs[2].message)
	}
	// The 4 events are: playlist, seg 1/3, seg 2/3, seg 3/3 — the explicit
	// "done" phase is deliberately suppressed (the {result} that follows is
	// the real ready signal), which the len==4 check above already pins.
	if msgs[0].message != "Preparazione flusso…" {
		t.Errorf("playlist message = %q", msgs[0].message)
	}
	if msgs[3].message != "Preparazione flusso… 3/3 segmenti" {
		t.Errorf("last message = %q; want the 3/3 segment tick", msgs[3].message)
	}
}

func TestPrebufferHLS_LiveUsesLiveTarget(t *testing.T) {
	t.Setenv("MYCELIUM_PREBUFFER_SEGMENTS_LIVE", "1")
	var gotTarget int
	swapHook(t, func(_ context.Context, _ string, opts managers.PrefetchOptions, _ func(managers.PrefetchProgress)) managers.PrefetchResult {
		gotTarget = opts.MaxSegments
		return managers.PrefetchResult{}
	})
	send, _ := captureProgress()
	prebufferHLS(context.Background(), "example.live", "http://cdn/live.m3u8", nil, true, send)
	if gotTarget != 1 {
		t.Fatalf("live MaxSegments = %d; want 1", gotTarget)
	}
}

func TestPrebufferHLS_DisabledByConfigSkipsHook(t *testing.T) {
	t.Setenv("MYCELIUM_PREBUFFER_ENABLED", "0")
	called := false
	swapHook(t, func(_ context.Context, _ string, _ managers.PrefetchOptions, _ func(managers.PrefetchProgress)) managers.PrefetchResult {
		called = true
		return managers.PrefetchResult{}
	})
	send, get := captureProgress()
	prebufferHLS(context.Background(), "example.movie", "http://cdn/x.m3u8", nil, false, send)
	if called {
		t.Fatalf("hook ran despite prebuffer_enabled=0")
	}
	if len(get()) != 0 {
		t.Fatalf("progress emitted despite disabled: %+v", get())
	}
}

func TestPrebufferHLS_HeartbeatDedupedToWaitingMessage(t *testing.T) {
	swapHook(t, func(_ context.Context, _ string, _ managers.PrefetchOptions, emit func(managers.PrefetchProgress)) managers.PrefetchResult {
		emit(managers.PrefetchProgress{Phase: "segment", Done: 1, Total: 4})
		emit(managers.PrefetchProgress{Phase: "segment", Done: 1, Total: 4}) // heartbeat, no advance
		emit(managers.PrefetchProgress{Phase: "segment", Done: 2, Total: 4})
		return managers.PrefetchResult{Segments: 2}
	})
	send, get := captureProgress()
	prebufferHLS(context.Background(), "example.series", "http://cdn/x.m3u8", nil, false, send)

	msgs := get()
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages; got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].message != "Preparazione flusso… 1/4 segmenti" {
		t.Errorf("msg[0] = %q", msgs[0].message)
	}
	if !strings.Contains(msgs[1].message, "attesa CDN") {
		t.Errorf("stalled heartbeat msg[1] = %q; want an 'attesa CDN' variant", msgs[1].message)
	}
	if msgs[2].message != "Preparazione flusso… 2/4 segmenti" {
		t.Errorf("msg[2] = %q", msgs[2].message)
	}
}

func TestPrebufferHLS_NilHookIsNoop(t *testing.T) {
	swapHook(t, nil)
	send, get := captureProgress()
	// must not panic
	prebufferHLS(context.Background(), "example.movie", "http://cdn/x.m3u8", nil, false, send)
	if len(get()) != 0 {
		t.Fatalf("nil hook still produced progress: %+v", get())
	}
}

func TestPrebufferHLS_StopsEmittingAfterCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	swapHook(t, func(c context.Context, _ string, _ managers.PrefetchOptions, emit func(managers.PrefetchProgress)) managers.PrefetchResult {
		emit(managers.PrefetchProgress{Phase: "segment", Done: 1, Total: 5})
		cancel()
		// A well-behaved emitter would stop here; simulate one more tick to
		// prove prebufferHLS's own ctx.Err() guard swallows it.
		emit(managers.PrefetchProgress{Phase: "segment", Done: 2, Total: 5})
		return managers.PrefetchResult{Segments: 1}
	})
	send, get := captureProgress()
	prebufferHLS(ctx, "example.movie", "http://cdn/x.m3u8", nil, false, send)

	msgs := get()
	if len(msgs) != 1 {
		t.Fatalf("emitted %d messages after cancel; want 1: %+v", len(msgs), msgs)
	}
}

// Guard: the whole call is quick even if the hook is (it shouldn't block the
// RPC handler goroutine beyond the hook's own work).
func TestPrebufferHLS_ReturnsPromptly(t *testing.T) {
	swapHook(t, func(_ context.Context, _ string, _ managers.PrefetchOptions, _ func(managers.PrefetchProgress)) managers.PrefetchResult {
		time.Sleep(20 * time.Millisecond)
		return managers.PrefetchResult{}
	})
	send, _ := captureProgress()
	start := time.Now()
	prebufferHLS(context.Background(), "example.movie", "http://cdn/x.m3u8", nil, false, send)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("prebufferHLS blocked for %s", d)
	}
}
