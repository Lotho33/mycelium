package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/core"
)

// A live token expires mid-stream: the player keeps reloading the top-level
// URL it was given. The proxy recovers in place (no redirect) and later
// reloads go straight to the new upstream.
func TestProxyPlaylist_TopLevelRecoveredInPlace(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)

	var oldHits, newHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/live/index.m3u8":
			if r.URL.Query().Get("t") == "old" {
				atomic.AddInt32(&oldHits, 1)
				http.Error(w, "token required", http.StatusForbidden)
				return
			}
			atomic.AddInt32(&newHits, 1)
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:7\n#EXTINF:4,\nseg7.ts\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	sid := b64url("plug\x00top-" + t.Name())
	reResolveMu.Lock()
	reResolveCache[sid+"\x00"] = &reResolveEntry{at: time.Now(), url: up.URL + "/live/index.m3u8?t=new", headers: map[string]string{"Referer": "https://player.example/"}}
	reResolveMu.Unlock()
	defer func() { reResolveMu.Lock(); delete(reResolveCache, sid+"\x00"); reResolveMu.Unlock() }()

	topURL := signedPlaylistURL(up.URL+"/live/index.m3u8?t=old", "&sid="+sid)
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, topURL, nil))
		if rec.Code != 200 {
			t.Fatalf("reload %d: status %d (want 200 in place, no redirect) body %q", i, rec.Code, rec.Body.String())
		}
		segs := childURLs(t, rec.Body.String())
		if len(segs) != 1 || proxyB64Decode(segs[0].Query().Get("data")) != up.URL+"/live/seg7.ts" {
			t.Fatalf("reload %d: segments %v", i, segs)
		}
		if got := proxyB64Decode(segs[0].Query().Get("origin")); got != "https://player.example/" {
			t.Errorf("segment lost the re-resolved referer: %q", got)
		}
	}
	if oldHits != 1 {
		t.Errorf("expired upstream hit %d times, want 1 (later reloads must use the remap)", oldHits)
	}
	// The third reload may come from the proxy's short live-playlist cache.
	if newHits < 2 {
		t.Errorf("new upstream hit %d times, want at least 2", newHits)
	}
}

// Without a stream id there is nothing to re-resolve: the error stands.
func TestProxyPlaylist_TopLevelFailureWithoutSid(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer up.Close()
	rec := httptest.NewRecorder()
	ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, signedPlaylistURL(up.URL+"/x/index.m3u8", ""), nil))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Header().Get("Location"), "playlist") {
		t.Fatalf("status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A plugin whose playlist URL never changes (auth in a cookie): after one
// recovery, a fresh Play of the same URL with its own, newer cookie must go
// to the upstream with that cookie, not follow the earlier recovery.
func TestProxyPlaylist_TopRemapDoesNotCaptureFreshResolve(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)

	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.Header.Get("Cookie")
		seen = append(seen, c)
		if c == "tok=1" {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nseg.ts\n")
	}))
	defer up.Close()

	sid := b64url("plug\x00fixed-" + t.Name())
	reResolveMu.Lock()
	reResolveCache[sid+"\x00"] = &reResolveEntry{at: time.Now(), url: up.URL + "/live.m3u8", headers: map[string]string{"Cookie": "tok=2"}}
	reResolveMu.Unlock()
	defer func() { reResolveMu.Lock(); delete(reResolveCache, sid+"\x00"); reResolveMu.Unlock() }()

	with := func(cookie string) string {
		return core.AppendProxySig("http://box.local/proxy/playlist.m3u8?data=" + b64url(up.URL+"/live.m3u8") +
			"&origin=&cookies=" + b64url(cookie) + "&vpn=0&sid=" + sid)
	}
	// The first Play's cookie expires: recovered with tok=2.
	rec := httptest.NewRecorder()
	ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, with("tok=1"), nil))
	if rec.Code != 200 {
		t.Fatalf("recovery: status %d", rec.Code)
	}
	// A fresh Play of the same URL with tok=3: straight to the upstream with tok=3.
	seen = nil
	rec = httptest.NewRecorder()
	ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, with("tok=3"), nil))
	if rec.Code != 200 || len(seen) != 1 || seen[0] != "tok=3" {
		t.Fatalf("fresh Play: status %d, upstream saw cookies %v (want only tok=3)", rec.Code, seen)
	}
}

// A playlist that is really broken (gone upstream): one recovery attempt,
// then 502 — no redirect fetching the same upstream once more.
func TestProxyPlaylist_TopLevelRealFailureIsOneAttempt(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)

	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer up.Close()

	sid := b64url("plug\x00gone-" + t.Name())
	reResolveMu.Lock()
	reResolveCache[sid+"\x00"] = &reResolveEntry{at: time.Now(), url: up.URL + "/gone.m3u8?v=2"}
	reResolveMu.Unlock()
	defer func() { reResolveMu.Lock(); delete(reResolveCache, sid+"\x00"); reResolveMu.Unlock() }()

	// Attempts one fetch makes on its own (its retries), to count fetches.
	plClient, err := upstreamPlaylistClient(false, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fetchUpstreamPlaylist(plClient, up.URL+"/probe.m3u8", "", "", "", false, "")
	perFetch := atomic.SwapInt32(&hits, 0)

	rec := httptest.NewRecorder()
	ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, signedPlaylistURL(up.URL+"/gone.m3u8?v=1", "&sid="+sid), nil))
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Location") != "" {
		t.Fatalf("status %d location %q, want 502 without redirect", rec.Code, rec.Header().Get("Location"))
	}
	// The original fetch and the re-resolved one: no third via a redirect.
	if n := atomic.LoadInt32(&hits); n != 2*perFetch {
		t.Fatalf("upstream hit %d times, want %d (2 fetches × %d attempts)", n, 2*perFetch, perFetch)
	}
}
