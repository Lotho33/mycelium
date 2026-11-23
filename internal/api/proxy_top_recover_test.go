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
