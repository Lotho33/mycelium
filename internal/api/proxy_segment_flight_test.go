package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// A player that gives up on a slow segment and asks again must get the bytes
// of the first, still running, upstream fetch — not start a second one.
func TestProxySegment_RetryAfterPlayerTimeoutReusesTheFetch(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)

	ts := fakeTS(20)
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(300 * time.Millisecond) // slower than the "player" waits
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(ts)
	}))
	defer upstream.Close()
	segURL := signedSegmentURL(upstream.URL + "/slow.ts")

	// First request: the player disconnects after 100ms.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel1()
	first := make(chan struct{})
	go func() {
		defer close(first)
		ProxySegment(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, segURL, nil).WithContext(ctx1))
	}()

	// Its retry arrives while the upstream fetch is still running.
	time.Sleep(150 * time.Millisecond)
	rec := httptest.NewRecorder()
	ProxySegment(rec, httptest.NewRequest(http.MethodGet, segURL, nil))
	<-first

	if rec.Code != http.StatusOK || rec.Body.Len() != len(ts) {
		t.Fatalf("retry: status %d, %d bytes; want 200 with %d bytes", rec.Code, rec.Body.Len(), len(ts))
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (the retry must reuse the running fetch)", got)
	}
}

// A first attempt that gets no headers in time is retried within the budget.
func TestProxySegment_FirstAttemptTTFBRetries(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)

	oldFirst := segmentFirstAttemptTTFB
	segmentFirstAttemptTTFB = 80 * time.Millisecond
	defer func() { segmentFirstAttemptTTFB = oldFirst }()

	ts := fakeTS(10)
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			<-r.Context().Done() // stuck connection
			return
		}
		w.Write(ts)
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	start := time.Now()
	ProxySegment(rec, httptest.NewRequest(http.MethodGet, signedSegmentURL(upstream.URL+"/stuck.ts"), nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != len(ts) {
		t.Fatalf("status %d, %d bytes; want 200 with %d bytes", rec.Code, rec.Body.Len(), len(ts))
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s, want the stuck first attempt cut at ~80ms", d)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2", got)
	}
}
