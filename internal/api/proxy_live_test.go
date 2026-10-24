package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// Two players of one live stream: the playlist goes upstream once per short
// interval, the newest segments are pulled into RAM once, and both players
// get them from there.
func TestLive_PlayersShareOneUpstreamFlow(t *testing.T) {
	var playlistHits atomic.Int32
	segHits := map[string]*atomic.Int32{}
	for i := 0; i < 6; i++ {
		segHits[fmt.Sprintf("seg%d.ts", i)] = &atomic.Int32{}
	}
	ts := fakeTS(20)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			playlistHits.Add(1)
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:0\n")
			for i := 0; i < 6; i++ {
				fmt.Fprintf(&b, "#EXTINF:6,\nseg%d.ts\n", i)
			}
			w.Write([]byte(b.String()))
			return
		}
		segHits[strings.TrimPrefix(r.URL.Path, "/v/")].Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(ts)
	}))
	defer up.Close()
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	t.Cleanup(func() { core.SetProxySignKey(nil) })
	useLoopbackPlaylistClient(t)
	useLoopbackSegmentClient(t)
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)
	livePlaylists.Lock()
	livePlaylists.m = map[string]livePlaylist{}
	livePlaylists.Unlock()

	plURL := core.AppendProxySig("http://box.local/proxy/playlist.m3u8?data=" + b64url(up.URL+"/v/live.m3u8") + "&origin=&cookies=&vpn=0")
	for player := 0; player < 2; player++ {
		rec := httptest.NewRecorder()
		ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, plURL, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/proxy/segment.ts") {
			t.Fatalf("player %d playlist: %d\n%s", player, rec.Code, rec.Body.String())
		}
	}
	if n := playlistHits.Load(); n != 1 {
		t.Fatalf("live playlist fetched upstream %d times, want 1", n)
	}
	tail := []string{up.URL + "/v/seg3.ts", up.URL + "/v/seg4.ts", up.URL + "/v/seg5.ts"}
	waitCached(t, tail...)
	if segHits["seg0.ts"].Load() != 0 {
		t.Fatal("segments behind the live edge were fetched")
	}
	// Both players take the tail from RAM.
	for player := 0; player < 2; player++ {
		for _, u := range tail {
			rec := httptest.NewRecorder()
			ProxySegment(rec, httptest.NewRequest(http.MethodGet, signedSegmentURL(u), nil))
			if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
				t.Fatalf("player %d %s: %d", player, u, rec.Code)
			}
		}
	}
	for _, name := range []string{"seg3.ts", "seg4.ts", "seg5.ts"} {
		if n := segHits[name].Load(); n != 1 {
			t.Fatalf("%s fetched upstream %d times, want 1", name, n)
		}
	}
}

func TestLivePlaylistTTL(t *testing.T) {
	for _, c := range []struct {
		body string
		want time.Duration
	}{
		{"#EXT-X-TARGETDURATION:6\n", 3 * time.Second},
		{"#EXT-X-TARGETDURATION:4\n", 2 * time.Second},
		{"#EXT-X-TARGETDURATION:1\n", time.Second},
		{"#EXT-X-TARGETDURATION:10\n", 3 * time.Second},
		{"", time.Second},
	} {
		if got := livePlaylistTTL([]byte(c.body)); got != c.want {
			t.Errorf("%q: %v, want %v", c.body, got, c.want)
		}
	}
	if isLiveMediaPlaylist([]byte("#EXTINF:6,\na.ts\n#EXT-X-ENDLIST\n")) || !isLiveMediaPlaylist([]byte("#EXTINF:6,\na.ts\n")) {
		t.Fatal("live detection")
	}
}
