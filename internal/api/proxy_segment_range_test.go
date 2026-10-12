package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// rangeFile serves data with full Range support, like a CDN or file host.
func rangeFile(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "f.mp4", time.Time{}, bytes.NewReader(data))
	}
}

func rangeGet(t *testing.T, u, rg string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func setupRangeTest(t *testing.T) {
	t.Helper()
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	t.Cleanup(func() { core.SetProxySignKey(nil) })
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)
}

// A player's byte range goes upstream, and the 206 comes back as it was.
func TestProxySegment_ForwardsPlayerRange(t *testing.T) {
	setupRangeTest(t)
	data := fakeTS(100)
	upstream := httptest.NewServer(rangeFile(data))
	defer upstream.Close()
	srv := proxyServer(t)

	resp := rangeGet(t, proxied(srv, upstream.URL+"/movie.mp4"), "bytes=1000-1999")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 1000-1999/%d", len(data)) {
		t.Fatalf("Content-Range = %q", cr)
	}
	if !bytes.Equal(body, data[1000:2000]) {
		t.Fatalf("got %d bytes, want the 1000 requested", len(body))
	}
	if _, ok := managers.Segments.Get(upstream.URL + "/movie.mp4"); ok {
		t.Fatal("a partial body must not reach the warm cache")
	}
}

// "bytes=0-" is a request for the whole body: the normal path (200).
func TestProxySegment_WholeRangeIsAPlainRequest(t *testing.T) {
	setupRangeTest(t)
	data := fakeTS(50)
	var sawRange atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			sawRange.Store(true)
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(data)
	}))
	defer upstream.Close()
	srv := proxyServer(t)

	resp := rangeGet(t, proxied(srv, upstream.URL+"/s.ts"), "bytes=0-")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) || sawRange.Load() {
		t.Fatalf("status %d, %d bytes, range forwarded=%v; want 200, whole body, no range", resp.StatusCode, len(body), sawRange.Load())
	}
}

// A range past the end is the player's answer (416), not an upstream failure
// to retry.
func TestProxySegment_UnsatisfiableRangePassesThrough(t *testing.T) {
	setupRangeTest(t)
	upstream := httptest.NewServer(rangeFile(fakeTS(10)))
	defer upstream.Close()
	srv := proxyServer(t)

	resp := rangeGet(t, proxied(srv, upstream.URL+"/movie.mp4"), "bytes=999999-")
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status %d, want 416", resp.StatusCode)
	}
}

// A ranged body cut mid-way resumes from the right offset in the file (the
// range's start plus what was already relayed).
func TestProxySegment_RangeResumesFromTheRightOffset(t *testing.T) {
	setupRangeTest(t)
	data := fakeTS(300)
	var second atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=1000-" {
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 1000-%d/%d", len(data)-1, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)-1000))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[1000:6000])
			w.(http.Flusher).Flush()
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		second.Store(r.Header.Get("Range"))
		rangeFile(data)(w, r)
	}))
	defer upstream.Close()
	srv := proxyServer(t)

	resp := rangeGet(t, proxied(srv, upstream.URL+"/movie.mp4"), "bytes=1000-")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(body, data[1000:]) {
		t.Fatalf("got %d bytes, want %d", len(body), len(data)-1000)
	}
	// The span's end comes from the first answer's Content-Range.
	if rg, want := second.Load(), fmt.Sprintf("bytes=6000-%d", len(data)-1); rg != want {
		t.Fatalf("resume range = %v, want %s", rg, want)
	}
}

// A body that stops sending (no error, no bytes) is closed after
// segmentBodyIdle and resumed.
func TestProxySegment_StalledBodyIsResumed(t *testing.T) {
	setupRangeTest(t)
	old := segmentBodyIdle
	segmentBodyIdle = 150 * time.Millisecond
	defer func() { segmentBodyIdle = old }()

	data := fakeTS(300)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			rangeFile(data)(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stalls until the proxy gives up on it
	}))
	defer upstream.Close()
	srv := proxyServer(t)

	resp := rangeGet(t, proxied(srv, upstream.URL+"/s.ts"), "")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("got %d bytes, want %d", len(body), len(data))
	}
}

// A streamed body that keeps flowing outlives the retry budget: the budget
// bounds getting an answer, not relaying it.
func TestProxySegment_StreamedBodyOutlivesTheRetryBudget(t *testing.T) {
	setupRangeTest(t)
	old := segmentRetryBudget
	segmentRetryBudget = 150 * time.Millisecond
	defer func() { segmentRetryBudget = old }()

	data := fakeTS(500)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		step := len(data) / 5
		for i := 0; i < 5; i++ {
			end := (i + 1) * step
			if i == 4 {
				end = len(data)
			}
			w.Write(data[i*step : end])
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer upstream.Close()
	srv := proxyServer(t)

	resp := rangeGet(t, proxied(srv, upstream.URL+"/s.ts"), "")
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("got %d bytes, want %d", len(body), len(data))
	}
}

// Alternate renditions are marked (rt=a) in a master, and their segments,
// like init sections, don't count toward the position estimate (nr=1).
func TestProxyPlaylist_MarksSegmentsThatDontCount(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		if strings.HasSuffix(r.URL.Path, "/master.m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"ita\",URI=\"audio/ita.m3u8\"\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=900000,AUDIO=\"a\"\nvideo/720p.m3u8\n")
			return
		}
		io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4,\n0001.m4s\n#EXT-X-ENDLIST\n")
	}))
	defer upstream.Close()

	get := func(u string) string {
		rec := httptest.NewRecorder()
		ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, u, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		return rec.Body.String()
	}
	master := get(core.AppendProxySig("http://box.local/proxy/playlist.m3u8?data=" + b64url(upstream.URL+"/x/master.m3u8") + "&origin=&cookies=&vpn=0"))
	var mediaLine, variantLine string
	for _, l := range strings.Split(master, "\n") {
		switch {
		case strings.HasPrefix(l, "#EXT-X-MEDIA"):
			mediaLine = l
		case strings.Contains(l, "/proxy/playlist.m3u8"):
			variantLine = l
		}
	}
	if !strings.Contains(mediaLine, "&rt=a") || strings.Contains(variantLine, "rt=a") {
		t.Fatalf("rt=a must mark the rendition only:\n%s", master)
	}

	child := func(rt string) string {
		return get(core.AppendProxySig("http://box.local/proxy/playlist.m3u8?data=" + b64url(upstream.URL+"/x/audio/ita.m3u8") + "&origin=&cookies=&vpn=0" + rt))
	}
	for _, c := range []struct {
		rt       string
		segCount bool
	}{{"&rt=a", false}, {"", true}} {
		for _, l := range strings.Split(child(c.rt), "\n") {
			switch {
			case strings.HasPrefix(l, "#EXT-X-MAP"):
				if !strings.Contains(l, "&nr=1") {
					t.Errorf("rt=%q: init section must carry nr=1: %s", c.rt, l)
				}
			case strings.Contains(l, "/proxy/segment.ts"):
				if strings.Contains(l, "&nr=1") == c.segCount {
					t.Errorf("rt=%q: segment nr=1 is %v, want %v: %s", c.rt, !c.segCount, !c.segCount, l)
				}
			}
		}
	}
}
