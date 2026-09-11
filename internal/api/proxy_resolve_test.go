package api

import (
	"net/url"
	"strings"
	"testing"

	"mycelium/internal/core"
)

// Regression: a re-resolved playlist URL (the 302 target ProxyPlaylist issues
// when the CDN token expires mid-stream) must carry a valid signature, or
// requireProxySig rejects the player's follow-up request with 403 — silently
// breaking the entire "expired token, re-resolve and retry" path. The P1-2
// HMAC pass first missed this one mint site.
func TestBuildResolvedPlaylistURL_IsSigned(t *testing.T) {
	core.SetProxySignKey([]byte("test-master-secret"))
	defer core.SetProxySignKey(nil)

	got, origin := buildResolvedPlaylistURL(
		"https", "box.local",
		"vix.movie", "12345",
		"https://cdn.example.com/hls/master.m3u8?token=abc",
		map[string]string{"Referer": "https://vix.example/watch", "Cookie": "sess=1"},
		"user-1",
	)

	if origin != "https://vix.example/watch" {
		t.Errorf("origin = %q, want the resolved Referer", origin)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("returned URL doesn't parse: %v", err)
	}
	if u.Query().Get("sig") == "" {
		t.Fatal("returned URL has no sig param — the bug this test guards against")
	}
	if !core.VerifyProxyURL(u.Query()) {
		t.Fatal("returned URL's signature does not verify")
	}
	if !strings.Contains(got, "/proxy/playlist.m3u8?") {
		t.Errorf("unexpected URL shape: %s", got)
	}
	if u.Query().Get("rr") != "1" {
		t.Error("missing rr=1 marker — a second failed fetch should 502, not loop re-resolving")
	}
}

func TestBuildResolvedPlaylistURL_UnsignedWithoutKey(t *testing.T) {
	core.SetProxySignKey(nil)

	got, _ := buildResolvedPlaylistURL("http", "box.local", "p", "s", "https://cdn/x.m3u8", nil, "")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// No key configured: AppendProxySig still appends a sig (HMAC of an empty
	// key), but VerifyProxyURL must fail closed regardless.
	if core.VerifyProxyURL(u.Query()) {
		t.Fatal("verified with no signing key ever configured")
	}
}
