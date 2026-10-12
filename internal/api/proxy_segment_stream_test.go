package api

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
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

// tsTailGuard, fed in arbitrary chunks, must produce what trimTSTail does on
// the whole body.
func TestTSTailGuard_MatchesTrimTSTail(t *testing.T) {
	ts := fakeTS(40)
	corrupt := append([]byte(nil), ts...)
	corrupt[20*188] = 0x00 // one bad packet mid-way, sync resumes after it
	cases := map[string][]byte{
		"clean":          ts,
		"junk tail":      append(append([]byte(nil), ts...), bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 900)...),
		"partial packet": append(append([]byte(nil), ts...), ts[:100]...),
		"corrupt middle": corrupt,
		"bad last":       append(append([]byte(nil), ts...), make([]byte, 188)...),
	}
	r := rand.New(rand.NewSource(7))
	for name, body := range cases {
		v := segVerdict{body: body, ct: "video/MP2T"}
		v.trimTSTail()
		for round := 0; round < 20; round++ {
			var out bytes.Buffer
			g := &tsTailGuard{out: &out}
			for rest := body; len(rest) > 0; {
				n := 1 + r.Intn(3000)
				if n > len(rest) {
					n = len(rest)
				}
				g.Write(rest[:n])
				rest = rest[n:]
			}
			g.finish()
			if !bytes.Equal(out.Bytes(), v.body) {
				t.Fatalf("%s (round %d): guard gave %d bytes, trimTSTail %d", name, round, out.Len(), len(v.body))
			}
		}
	}
}

func TestStreamableSegment(t *testing.T) {
	ts := fakeTS(10)
	png := append([]byte("\x89PNG\r\n\x1a\n"), ts...)
	cases := []struct {
		name, ct string
		head     []byte
		enc      bool
		ok       bool
		want     string
	}{
		{"ts", "video/mp2t", ts, false, true, "video/mp2t"},
		{"ts mislabelled", "text/html", ts, false, true, "video/MP2T"},
		{"fmp4", "", append([]byte{0, 0, 0, 24}, []byte("styp")...), false, true, "video/mp4"},
		{"adts audio", "application/octet-stream", []byte{0xFF, 0xF1, 0x50, 0x80}, false, true, "audio/aac"},
		{"png wrapper", "image/png", png, false, false, ""},
		{"html page", "text/html", []byte("<html><body>denied</body></html>"), false, false, ""},
		{"ciphertext", "video/mp2t", bytes.Repeat([]byte{0x9a}, 64), true, true, "video/MP2T"},
		{"wrapped ciphertext", "image/png", png, true, false, ""},
	}
	for _, c := range cases {
		got, ok := streamableSegment(c.head, c.ct, "https://cdn.example/seg.ts", c.enc)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// proxyServer serves ProxySegment over a real listener (the streaming path
// needs a live connection to show bytes arriving before the end).
func proxyServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(ProxySegment))
	t.Cleanup(srv.Close)
	return srv
}

func proxied(srv *httptest.Server, upstreamURL string) string {
	return strings.Replace(signedSegmentURL(upstreamURL), "http://box.local", srv.URL, 1)
}

// A clean segment reaches the player while the CDN is still sending it.
func TestProxySegment_StreamsCleanSegmentAsItArrives(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)

	ts := fakeTS(200)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(ts[:len(ts)/2])
		w.(http.Flusher).Flush()
		<-release
		w.Write(ts[len(ts)/2:])
	}))
	defer upstream.Close()
	srv := proxyServer(t)

	resp, err := http.Get(proxied(srv, upstream.URL+"/seg.ts"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 4096)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, first); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("no bytes reached the player before the upstream body ended")
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := append(first, rest...); !bytes.Equal(got, ts) {
		t.Fatalf("got %d bytes, want the %d-byte segment intact", len(got), len(ts))
	}
}

// cutAfter answers with a Content-Length for the whole body but drops the
// connection after n bytes.
func cutAfter(w http.ResponseWriter, body []byte, n int) {
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body[:n])
	w.(http.Flusher).Flush()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err == nil {
		conn.Close()
	}
}

// A body cut mid-way is completed with a Range request from where it
// stopped; the player sees one intact segment.
func TestProxySegment_ResumesCutBodyWithRange(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)

	ts := fakeTS(300)
	var ranges atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rg := r.Header.Get("Range")
		if rg == "" {
			cutAfter(w, ts, 20000)
			return
		}
		ranges.Store(rg)
		var from int
		fmt.Sscanf(rg, "bytes=%d-", &from)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(ts)-1, len(ts)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(ts[from:])
	}))
	defer upstream.Close()
	srv := proxyServer(t)

	resp, err := http.Get(proxied(srv, upstream.URL+"/seg.ts"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, ts) {
		t.Fatalf("got %d bytes, want the %d-byte segment intact", len(got), len(ts))
	}
	if rg, _ := ranges.Load().(string); rg != "bytes=20000-" {
		t.Fatalf("range request = %q, want bytes=20000-", rg)
	}
}

// Without a usable Range answer the player's connection is broken, so it
// sees an error and retries instead of playing a short segment.
func TestProxySegment_CutWithoutRangeBreaksTheConnection(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)

	ts := fakeTS(300)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Write(ts) // ignores the range: not a resumable answer
			return
		}
		cutAfter(w, ts, 20000)
	}))
	defer upstream.Close()
	srv := proxyServer(t)

	resp, err := http.Get(proxied(srv, upstream.URL+"/seg.ts"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatalf("read %d bytes with no error; want the connection broken mid-body", len(got))
	}
}

// freshConnHeader never reaches upstream, and the request still goes through
// on its one-off connection.
func TestDirectTransport_FreshConnMarkerStaysInternal(t *testing.T) {
	var sawMarker atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(freshConnHeader) != "" || r.Header.Get(pinUAHeader) != "" {
			sawMarker.Store(true)
		}
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	c := &http.Client{Transport: directTransport{stdTransport(upstreamTransportOpts{})}}
	for _, fresh := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodGet, upstream.URL, nil)
		req.Header.Set(pinUAHeader, "1")
		if fresh {
			req.Header.Set(freshConnHeader, "1")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("fresh=%v: %v", fresh, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "ok" {
			t.Fatalf("fresh=%v: body %q", fresh, b)
		}
	}
	if sawMarker.Load() {
		t.Fatal("an internal marker header reached upstream")
	}
}

// A player that hangs up mid-stream (typically to retry) doesn't cancel the
// upstream fetch: the leader completes it into the warm cache.
func TestProxySegment_PlayerLeavingMidStreamStillFillsTheCache(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)

	ts := fakeTS(400)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(ts[:len(ts)/4])
		w.(http.Flusher).Flush()
		<-release
		w.Write(ts[len(ts)/4:])
	}))
	defer upstream.Close()
	defer close(release)
	srv := proxyServer(t)
	segURL := upstream.URL + "/seg.ts"

	resp, err := http.Get(proxied(srv, segURL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // the player gives up mid-segment
	time.Sleep(100 * time.Millisecond)
	release <- struct{}{}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, ok := managers.Segments.Get(segURL); ok {
			if !bytes.Equal(b, ts) {
				t.Fatalf("cached %d bytes, want the %d-byte segment", len(b), len(ts))
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("segment never reached the warm cache after the player left")
}

// The CDN slot is freed once upstream is done, not once a slow player has
// read everything.
func TestProxySegment_SlowPlayerDoesNotHoldTheCDNSlot(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)
	t.Setenv("MYCELIUM_SEGMENT_CONCURRENCY", "1")

	big := fakeTS(40000) // ~7.5 MB: more than the socket buffers hold
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		if strings.HasSuffix(r.URL.Path, "/small.ts") {
			w.Write(fakeTS(10))
			return
		}
		w.Write(big)
	}))
	defer upstream.Close()
	// A host of its own: the gate map is process-wide.
	host := strings.Replace(upstream.URL, "127.0.0.1", "localhost", 1)
	srv := proxyServer(t)

	slow, err := http.Get(proxied(srv, host+"/big.ts"))
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Body.Close()
	io.ReadFull(slow.Body, make([]byte, 1024)) // then stops reading

	done := make(chan error, 1)
	go func() {
		resp, err := http.Get(proxied(srv, host+"/small.ts"))
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a second segment from the same CDN waited on a slot held by a slow player")
	}
}

// A retry arriving while the leader is still streaming reads along from
// byte 0 at once, instead of waiting for the end in silence.
func TestProxySegment_RetryReadsAlongTheStreamingLeader(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)

	ts := fakeTS(400)
	release := make(chan struct{})
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(ts[:len(ts)/4])
		w.(http.Flusher).Flush()
		<-release
		w.Write(ts[len(ts)/4:])
	}))
	defer upstream.Close()
	srv := proxyServer(t)
	segURL := proxied(srv, upstream.URL+"/seg.ts")

	first, err := http.Get(segURL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	if _, err := io.ReadFull(first.Body, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}

	// The retry gets the first quarter while upstream is still held.
	retry, err := http.Get(segURL)
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Body.Close()
	head := make([]byte, len(ts)/4)
	got := make(chan error, 1)
	go func() { _, err := io.ReadFull(retry.Body, head); got <- err }()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("the retry got nothing while the leader was streaming")
	}
	close(release)
	rest, err := io.ReadAll(retry.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b := append(head, rest...); !bytes.Equal(b, ts) {
		t.Fatalf("retry got %d bytes, want the %d-byte segment", len(b), len(ts))
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}
}
