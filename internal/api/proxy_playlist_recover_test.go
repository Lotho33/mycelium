package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

func signedPlaylistURL(upstreamURL, extra string) string {
	return core.AppendProxySig("http://box.local/proxy/playlist.m3u8?data=" + b64url(upstreamURL) + "&origin=&cookies=&vpn=0" + extra)
}

// childURLs returns the proxied playlist URLs a rewritten master points at.
func childURLs(t *testing.T, body string) []*url.URL {
	t.Helper()
	var out []*url.URL
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if m := reKeyURI.FindStringSubmatch(line); len(m) == 2 {
			line = m[1]
		} else if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			t.Fatalf("bad child url %q", line)
		}
		out = append(out, u)
	}
	return out
}

const testMaster = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="ita",URI="audio/ita.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,AUDIO="aud"
low/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3000000,AUDIO="aud"
high/index.m3u8
`

// The vi numbering the rewrite hands out must be the order masterChildURIs
// walks, or a recovery would swap the video variant for the audio track.
func TestProxyPlaylist_MasterChildrenCarrySidAndIndex(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, testMaster)
	}))
	defer up.Close()

	sid := b64url("plug\x00stream1")
	rec := httptest.NewRecorder()
	ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, signedPlaylistURL(up.URL+"/hls/master.m3u8", "&sid="+sid), nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	kids := childURLs(t, rec.Body.String())
	want := masterChildURIs(testMaster, up.URL+"/hls/")
	if len(kids) != len(want) || len(want) != 3 {
		t.Fatalf("children = %d, masterChildURIs = %d, want 3", len(kids), len(want))
	}
	for i, u := range kids {
		q := u.Query()
		if q.Get("sid") != sid || q.Get("vi") != string(rune('0'+i)) {
			t.Errorf("child %d: sid=%q vi=%q", i, q.Get("sid"), q.Get("vi"))
		}
		if got := proxyB64Decode(q.Get("data")); got != want[i] {
			t.Errorf("child %d points at %s, masterChildURIs says %s", i, got, want[i])
		}
		if !core.VerifyProxyURL(q) {
			t.Errorf("child %d signature does not verify", i)
		}
	}
}

// A media playlist's segments must not get sid/vi.
func TestProxyPlaylist_MediaPlaylistSegmentsHaveNoIndex(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nseg1.ts\n#EXTINF:4,\nseg2.ts\n")
	}))
	defer up.Close()
	rec := httptest.NewRecorder()
	ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, signedPlaylistURL(up.URL+"/v.m3u8", "&sid="+b64url("p\x00s")), nil))
	if strings.Contains(rec.Body.String(), "vi=") {
		t.Fatalf("media playlist segments got a vi:\n%s", rec.Body.String())
	}
}

// A variant whose upstream starts failing (expired token) is re-resolved in
// place: the player's URL keeps working, and later reloads go straight to
// the new upstream.
func TestProxyPlaylist_ChildRecoveredAfterReResolve(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)

	var oldHits, newHits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/old/"):
			atomic.AddInt32(&oldHits, 1)
			http.Error(w, "token expired", http.StatusForbidden)
		case r.URL.Path == "/new/master.m3u8":
			io.WriteString(w, testMaster)
		case r.URL.Path == "/new/high/index.m3u8":
			atomic.AddInt32(&newHits, 1)
			if r.Header.Get("Cookie") != "tok=2" {
				t.Errorf("new variant fetched without the re-resolved cookie: %q", r.Header.Get("Cookie"))
			}
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nseg9.ts\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	// Stand in for the plugin: a fresh re-resolve result for this sid.
	sid := b64url("plug\x00recover-" + t.Name())
	reResolveMu.Lock()
	reResolveCache[sid+"\x00"] = &reResolveEntry{at: time.Now(), url: up.URL + "/new/master.m3u8", headers: map[string]string{"Cookie": "tok=2"}}
	reResolveMu.Unlock()
	defer func() { reResolveMu.Lock(); delete(reResolveCache, sid+"\x00"); reResolveMu.Unlock() }()

	childURL := signedPlaylistURL(up.URL+"/old/high/index.m3u8", "&sid="+sid+"&vi=2")
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, childURL, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "segment.ts") {
			t.Fatalf("reload %d: status %d body %q", i, rec.Code, rec.Body.String())
		}
		for _, u := range childURLs(t, rec.Body.String()) {
			if got := proxyB64Decode(u.Query().Get("data")); got != up.URL+"/new/high/seg9.ts" {
				t.Errorf("segment points at %s", got)
			}
			if proxyB64Decode(u.Query().Get("cookies")) != "tok=2" {
				t.Error("segment lost the re-resolved cookie")
			}
		}
	}
	if oldHits != 1 {
		t.Errorf("old upstream hit %d times, want 1 (second reload must use the remap)", oldHits)
	}
	if newHits != 2 {
		t.Errorf("new variant hit %d times, want 2", newHits)
	}
}

// A connection drop on a playlist reload is retried, not turned into a 502.
func TestFetchUpstreamPlaylist_RetriesConnectionErrors(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		io.WriteString(w, "#EXTM3U\n")
	}))
	defer up.Close()
	b, err := fetchUpstreamPlaylist(&http.Client{}, up.URL+"/x.m3u8", "", "", "", false, "")
	if err != nil || string(b) != "#EXTM3U\n" {
		t.Fatalf("got %q, %v", b, err)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
}

// A segment whose upstream connection dies halfway is fetched again instead
// of reaching the player truncated.
func TestProxySegment_RetriesTruncatedBody(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)

	full := strings.Repeat("G", 188*50)
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/MP2T")
		w.Header().Set("Content-Length", "9400")
		if atomic.AddInt32(&hits, 1) == 1 {
			io.WriteString(w, full[:1000])
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		io.WriteString(w, full)
	}))
	defer up.Close()

	rec := httptest.NewRecorder()
	ProxySegment(rec, httptest.NewRequest(http.MethodGet, signedSegmentURL(up.URL+"/s.ts"), nil))
	if rec.Code != 200 || rec.Body.String() != full {
		t.Fatalf("status %d, body %d bytes (want %d)", rec.Code, rec.Body.Len(), len(full))
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
}

// A player whose profile has since started playing on another device gets
// 409 on its next fetch.
func TestProxySegment_RefusedAfterLeaseLost(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/MP2T")
		io.WriteString(w, strings.Repeat("G", 188))
	}))
	defer up.Close()

	profile := "prof-" + t.Name()
	token := "tok-" + t.Name()
	managers.Sessions.Register(token, &managers.StreamSession{UserID: profile, ProfileID: profile, DeviceID: "tv"})
	defer managers.Sessions.Remove(token)
	defer managers.PlaybackLeases.Release(profile)

	u := core.AppendProxySig("http://box.local/proxy/segment.ts?data=" + b64url(up.URL+"/s.ts") + "&origin=&cookies=&vpn=0&uid=" + token)

	managers.PlaybackLeases.Claim(profile, "tv")
	rec := httptest.NewRecorder()
	ProxySegment(rec, httptest.NewRequest(http.MethodGet, u, nil))
	if rec.Code != 200 {
		t.Fatalf("lease holder refused: %d", rec.Code)
	}

	managers.PlaybackLeases.Release(profile)
	managers.PlaybackLeases.Claim(profile, "phone")
	rec = httptest.NewRecorder()
	ProxySegment(rec, httptest.NewRequest(http.MethodGet, u, nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
}

func TestNonVideoSegmentType(t *testing.T) {
	for in, want := range map[string]string{
		"WEBVTT\n\n00:00.000 --> 00:01.000\nx": "text/vtt",
		"\xef\xbb\xbfWEBVTT\n":                 "text/vtt",
		"ID3\x04\x00":                          "audio/aac",
		"\xff\xf1\x50\x80":                     "audio/aac",
		"\xff\xfb\x90\x00":                     "audio/mpeg",
		"<html><body>blocked</body></html>":    "",
		"{\"error\":1}":                        "",
	} {
		if got := nonVideoSegmentType([]byte(in)); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// Subtitle and raw-audio segments pass through the segment proxy instead of
// being rejected as "non-video".
func TestProxySegment_RelaysSubtitlesAndAudio(t *testing.T) {
	core.SetProxySignKey([]byte("recover-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)
	bodies := map[string][2]string{
		"/s.vtt":  {"text/vtt", "WEBVTT\n\n00:01.000 --> 00:02.000\nCiao\n"},
		"/a.aac":  {"audio/aac", "\xff\xf1\x50\x80\x02\x1f\xfc"},
		"/b.webp": {"text/plain", "WEBVTT\n\n00:01.000 --> 00:02.000\nx\n"}, // mislabelled
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := bodies[r.URL.Path]
		w.Header().Set("Content-Type", b[0])
		io.WriteString(w, b[1])
	}))
	defer up.Close()
	for path, b := range bodies {
		rec := httptest.NewRecorder()
		ProxySegment(rec, httptest.NewRequest(http.MethodGet, signedSegmentURL(up.URL+path), nil))
		if rec.Code != 200 || rec.Body.String() != b[1] {
			t.Errorf("%s: status %d body %q", path, rec.Code, rec.Body.String())
		}
	}
}

// The proxy re-resolves with the profile behind the uid: a playback session's,
// or a download's owner (never a lease-holding session for downloads).
func TestSessionProfile_PlaybackAndDownloadOwners(t *testing.T) {
	managers.Sessions.Register("tok-play-"+t.Name(), &managers.StreamSession{ProfileID: "p1", DeviceID: "d1"})
	defer managers.Sessions.Remove("tok-play-" + t.Name())
	managers.ProxyOwners.Register("dl-"+t.Name(), "p2")
	if got := sessionProfile("tok-play-" + t.Name()); got != "p1" {
		t.Fatalf("playback uid → %q", got)
	}
	if got := sessionProfile("dl-" + t.Name()); got != "p2" {
		t.Fatalf("download uid → %q", got)
	}
	if _, ok := managers.Sessions.Get("dl-" + t.Name()); ok {
		t.Fatal("download uid must not be a playback session")
	}
	if sessionProfile("unknown") != "" || sessionProfile("") != "" {
		t.Fatal("unknown uid must map to no profile")
	}
}
