package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/managers"
)

// fakeUpstream serves a master → variant → N segments HLS tree and counts
// segment hits so tests can assert the warm cache actually short-circuits.
type fakeUpstream struct {
	srv       *httptest.Server
	segHits   int64
	segDelay  time.Duration // per-segment sleep, to exercise the wait cap
	nSegments int
}

func newFakeUpstream(t *testing.T, nSegments int) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{nSegments: nSegments}
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\nlo/media.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2400000\nhi/media.m3u8\n")
	})
	media := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		for i := 0; i < f.nSegments; i++ {
			fmt.Fprintf(&b, "#EXTINF:4.000,\nseg%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	}
	mux.HandleFunc("/hi/media.m3u8", media)
	mux.HandleFunc("/lo/media.m3u8", media)
	mux.HandleFunc("/hi/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".ts") {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&f.segHits, 1)
		if f.segDelay > 0 {
			select {
			case <-time.After(f.segDelay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "video/MP2T")
		// 0x47 sync byte + padding so it reads as a plausible TS packet.
		body := make([]byte, 376)
		body[0], body[188] = 0x47, 0x47
		copy(body[1:], []byte(r.URL.Path))
		w.Write(body)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) url(p string) string { return f.srv.URL + p }

// withFakeClients points the pre-fetcher's upstream clients at the test server.
func withFakeClients(t *testing.T, f *fakeUpstream) {
	t.Helper()
	c := f.srv.Client()
	c.Timeout = 5 * time.Second
	op, os := prefetchPlaylistClientFn, prefetchSegmentClientFn
	prefetchPlaylistClientFn = func(bool, string) (*http.Client, error) { return c, nil }
	prefetchSegmentClientFn = func(bool, string) (*http.Client, error) { return c, nil }
	t.Cleanup(func() {
		prefetchPlaylistClientFn, prefetchSegmentClientFn = op, os
	})
}

func collectEmits() (func(managers.PrefetchProgress), *[]managers.PrefetchProgress, *sync.Mutex) {
	var mu sync.Mutex
	var got []managers.PrefetchProgress
	return func(p managers.PrefetchProgress) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	}, &got, &mu
}

func TestPrefetchHLSHead_MasterVariantSegments(t *testing.T) {
	managers.Segments.Reset()
	f := newFakeUpstream(t, 5)
	withFakeClients(t, f)

	emit, got, mu := collectEmits()
	res := prefetchHLSHead(context.Background(), f.url("/master.m3u8"), managers.PrefetchOptions{
		MaxSegments: 3,
		MaxWait:     3 * time.Second,
	}, emit)

	if res.Err != nil {
		t.Fatalf("res.Err = %v", res.Err)
	}
	if res.Segments != 3 {
		t.Fatalf("res.Segments = %d; want 3", res.Segments)
	}
	if res.TimedOut {
		t.Fatalf("res.TimedOut = true unexpectedly")
	}

	// Warm cache: master + chosen (hi-bitrate) variant playlist + first 3 segs.
	mustHit := []string{
		f.url("/master.m3u8"),
		f.url("/hi/media.m3u8"),
		f.url("/hi/seg0.ts"), f.url("/hi/seg1.ts"), f.url("/hi/seg2.ts"),
	}
	for _, u := range mustHit {
		if _, ok := managers.Segments.Get(u); !ok {
			t.Errorf("warm cache missing %s", u)
		}
	}
	if _, ok := managers.Segments.Get(f.url("/hi/seg3.ts")); ok {
		t.Errorf("seg3 was pre-fetched despite MaxSegments=3")
	}

	// Progress: a "playlist" phase, monotonic "segment" Done, ending "done".
	mu.Lock()
	defer mu.Unlock()
	if len(*got) < 3 {
		t.Fatalf("too few progress events: %+v", *got)
	}
	if (*got)[0].Phase != "playlist" {
		t.Errorf("first phase = %q; want playlist", (*got)[0].Phase)
	}
	last := *got // final event
	if l := last[len(last)-1]; l.Phase != "done" || l.Done != 3 {
		t.Errorf("final event = %+v; want phase=done done=3", l)
	}
	maxDone := 0
	for _, p := range *got {
		if p.Phase == "segment" && p.Done < maxDone {
			t.Errorf("segment Done went backwards: %+v", *got)
		}
		if p.Done > maxDone {
			maxDone = p.Done
		}
		// Total is only known once the playlist is parsed.
		if (p.Phase == "segment" || p.Phase == "done") && p.Total != 3 {
			t.Errorf("event Total = %d; want 3 (%+v)", p.Total, p)
		}
	}
}

func TestPrefetchHLSHead_WaitCapReturnsPartial(t *testing.T) {
	managers.Segments.Reset()
	f := newFakeUpstream(t, 5)
	f.segDelay = 400 * time.Millisecond
	withFakeClients(t, f)

	emit, _, _ := collectEmits()
	start := time.Now()
	res := prefetchHLSHead(context.Background(), f.url("/master.m3u8"), managers.PrefetchOptions{
		MaxSegments: 5,
		MaxWait:     300 * time.Millisecond,
	}, emit)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("prefetch hung past its wait cap: %s", elapsed)
	}
	if !res.TimedOut {
		t.Errorf("res.TimedOut = false; want true (segments=%d err=%v)", res.Segments, res.Err)
	}
	if res.Segments >= 5 {
		t.Errorf("res.Segments = %d; expected a partial result under the cap", res.Segments)
	}
}

func TestPrefetchHLSHead_ContextCancelIsHonored(t *testing.T) {
	managers.Segments.Reset()
	f := newFakeUpstream(t, 8)
	f.segDelay = 300 * time.Millisecond
	withFakeClients(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	emit := func(p managers.PrefetchProgress) {
		if p.Phase == "segment" && p.Done == 1 {
			cancel() // bail out right after the first segment
		}
	}
	start := time.Now()
	res := prefetchHLSHead(ctx, f.url("/master.m3u8"), managers.PrefetchOptions{
		MaxSegments: 8,
		MaxWait:     5 * time.Second,
	}, emit)

	if time.Since(start) > 2*time.Second {
		t.Fatalf("prefetch did not stop promptly on cancel: %s", time.Since(start))
	}
	if res.Segments >= 8 {
		t.Errorf("prefetch ran to completion despite cancel: segments=%d", res.Segments)
	}
}

func TestProxySegmentServesWarmCacheWithoutUpstream(t *testing.T) {
	managers.Segments.Reset()
	f := newFakeUpstream(t, 3)
	withFakeClients(t, f)

	_ = prefetchHLSHead(context.Background(), f.url("/master.m3u8"), managers.PrefetchOptions{
		MaxSegments: 2,
		MaxWait:     3 * time.Second,
	}, nil)

	hitsAfterPrefetch := atomic.LoadInt64(&f.segHits)

	seg := f.url("/hi/seg0.ts")
	req := httptest.NewRequest(http.MethodGet, "/proxy/segment.ts?data="+prebufB64(seg), nil)
	rec := httptest.NewRecorder()
	ProxySegment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("ProxySegment status = %d; want 200", rec.Code)
	}
	if got := rec.Body.Len(); got != 376 {
		t.Fatalf("warm body len = %d; want 376", got)
	}
	if atomic.LoadInt64(&f.segHits) != hitsAfterPrefetch {
		t.Fatalf("ProxySegment hit upstream on a warm-cache URL (hits %d → %d)",
			hitsAfterPrefetch, atomic.LoadInt64(&f.segHits))
	}
}
