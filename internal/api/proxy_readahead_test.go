package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// raUpstream serves a media playlist of n 10s segments (VOD unless live) and
// the segments themselves, counting hits per segment.
type raUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits map[string]int
	slow map[string]chan struct{} // segment → released when closed
}

func newRAUpstream(t *testing.T, n int, live bool) *raUpstream {
	t.Helper()
	u := &raUpstream{hits: map[string]int{}, slow: map[string]chan struct{}{}}
	ts := fakeTS(20)
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:10\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "#EXTINF:10,\nseg%d.ts\n", i)
			}
			if !live {
				b.WriteString("#EXT-X-ENDLIST\n")
			}
			w.Write([]byte(b.String()))
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v/")
		u.mu.Lock()
		u.hits[name]++
		ch := u.slow[name]
		u.mu.Unlock()
		if ch != nil {
			<-ch
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(ts)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *raUpstream) count(name string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[name]
}

func (u *raUpstream) seg(i int) string { return fmt.Sprintf("%s/v/seg%d.ts", u.srv.URL, i) }

func setupReadAhead(t *testing.T, u *raUpstream) {
	t.Helper()
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	t.Cleanup(func() { core.SetProxySignKey(nil) })
	useLoopbackPlaylistClient(t)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)
	rec := httptest.NewRecorder()
	ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, core.AppendProxySig("http://box.local/proxy/playlist.m3u8?data="+b64url(u.srv.URL+"/v/index.m3u8")+"&origin=&cookies=&vpn=0"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("playlist status %d", rec.Code)
	}
}

func waitCached(t *testing.T, urls ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, u := range urls {
			if !managers.Segments.Has(u) {
				all = false
			}
		}
		if all {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("read-ahead never cached %v", urls)
}

// A segment request fetches what follows into the warm cache, and the
// player's next requests are served from it without going upstream again.
func TestReadAhead_FetchesTheNextSegments(t *testing.T) {
	u := newRAUpstream(t, 8, false)
	setupReadAhead(t, u)

	ProxySegment(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, signedSegmentURL(u.seg(0)), nil))
	waitCached(t, u.seg(1), u.seg(2)) // raPerList at a time

	rec := httptest.NewRecorder()
	ProxySegment(rec, httptest.NewRequest(http.MethodGet, signedSegmentURL(u.seg(1)), nil))
	if rec.Code != http.StatusOK || u.count("seg1.ts") != 1 {
		t.Fatalf("seg1: status %d, upstream hits %d; want a cache hit after one read-ahead fetch", rec.Code, u.count("seg1.ts"))
	}
	waitCached(t, u.seg(3)) // the window moved on
}

// A live playlist (no #EXT-X-ENDLIST) gets no read-ahead.
func TestReadAhead_SkipsLivePlaylists(t *testing.T) {
	u := newRAUpstream(t, 8, true)
	setupReadAhead(t, u)

	ProxySegment(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, signedSegmentURL(u.seg(0)), nil))
	time.Sleep(200 * time.Millisecond)
	if n := u.count("seg1.ts") + u.count("seg2.ts"); n != 0 {
		t.Fatalf("live playlist was read ahead (%d fetches)", n)
	}
}

// A player asking for a segment the read-ahead is still fetching waits for
// that fetch instead of starting another.
func TestReadAhead_PlayerJoinsTheFetchUnderWay(t *testing.T) {
	u := newRAUpstream(t, 8, false)
	release := make(chan struct{})
	u.slow["seg1.ts"] = release
	setupReadAhead(t, u)

	ProxySegment(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, signedSegmentURL(u.seg(0)), nil))
	deadline := time.Now().Add(3 * time.Second)
	for u.count("seg1.ts") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	done := make(chan *httptest.ResponseRecorder)
	go func() {
		rec := httptest.NewRecorder()
		ProxySegment(rec, httptest.NewRequest(http.MethodGet, signedSegmentURL(u.seg(1)), nil))
		done <- rec
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	rec := <-done
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("status %d, %d bytes", rec.Code, rec.Body.Len())
	}
	if n := u.count("seg1.ts"); n != 1 {
		t.Fatalf("seg1 upstream hits = %d, want 1", n)
	}
}

// prebuffer_readahead_secs=0 turns it off.
func TestReadAhead_Disabled(t *testing.T) {
	t.Setenv("MYCELIUM_PREBUFFER_READAHEAD_SECS", "0")
	u := newRAUpstream(t, 8, false)
	setupReadAhead(t, u)

	ProxySegment(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, signedSegmentURL(u.seg(0)), nil))
	time.Sleep(200 * time.Millisecond)
	var fetched atomic.Int32
	for i := 1; i < 8; i++ {
		fetched.Add(int32(u.count(fmt.Sprintf("seg%d.ts", i))))
	}
	if fetched.Load() != 0 {
		t.Fatalf("read-ahead ran while disabled (%d fetches)", fetched.Load())
	}
}
