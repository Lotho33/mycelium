package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"mycelium/internal/managers"
)

func TestProxyRequestsAreBodyless(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	req, err := buildProxyRequest("GET", srv.URL+"/playlist.m3u8", srv.URL+"/embed", "", "")
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}
	if req.Body != nil {
		t.Errorf("buildProxyRequest produced a non-nil Body")
	}
}

// The cobweb relay: an upstream GET becomes a POST to cobweb's /v1/fetch, with
// request-shaping headers forwarded and the UA dropped unless a cf_clearance
// cookie rides along; cobweb's response is passed straight back.
func TestCobwebRoundTripper_ForwardsAsFetchAndDropsUA(t *testing.T) {
	var gotPath, gotCT string
	var gotBody map[string]any
	cobweb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("X-Cobweb-Final-Url", "https://cdn.example/final.ts")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, "SEGMENT-BYTES")
	}))
	defer cobweb.Close()
	t.Setenv("COBWEB_ADDR", cobweb.URL)

	rt := &cobwebRoundTripper{proxyURL: "socks5://vpn:1080"}
	req, _ := http.NewRequest(http.MethodGet, "https://cdn.example/seg/1.ts?tok=abc", nil)
	req.Header.Set("Referer", "https://player.example/")
	req.Header.Set("User-Agent", "Chrome/146 fake")
	req.Header.Set("Sec-Ch-Ua", `"Chromium";v="146"`)
	req.Header.Set("Range", "bytes=0-1023")
	req.Header.Set("Connection", "keep-alive")

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotPath != "/v1/fetch" {
		t.Errorf("cobweb path = %q, want /v1/fetch", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotBody["url"] != "https://cdn.example/seg/1.ts?tok=abc" {
		t.Errorf("forwarded url = %v", gotBody["url"])
	}
	if gotBody["proxy_url"] != "socks5://vpn:1080" {
		t.Errorf("proxy_url = %v", gotBody["proxy_url"])
	}
	hdrs, _ := gotBody["headers"].(map[string]any)
	if hdrs["Referer"] != "https://player.example/" {
		t.Errorf("Referer not forwarded: %v", hdrs)
	}
	if hdrs["Range"] != "bytes=0-1023" {
		t.Errorf("Range not forwarded: %v", hdrs)
	}
	if _, ok := hdrs["User-Agent"]; ok {
		t.Errorf("User-Agent should be dropped (no cf_clearance): %v", hdrs)
	}
	if _, ok := hdrs["Sec-Ch-Ua"]; ok {
		t.Errorf("Sec-Ch-Ua should be dropped with the UA: %v", hdrs)
	}
	if _, ok := hdrs["Connection"]; ok {
		t.Errorf("hop-by-hop Connection should be dropped: %v", hdrs)
	}
	if resp.StatusCode != 206 {
		t.Errorf("status = %d, want 206 (mirrored)", resp.StatusCode)
	}
	if string(body) != "SEGMENT-BYTES" {
		t.Errorf("body = %q", body)
	}
}

// The cobweb sidecar is a separate, non-auditable-from-here process; the
// RoundTripper must refuse to relay a fetch for an SSRF-blocked target rather
// than trusting cobweb to reject it, and must never even reach cobweb for one.
func TestCobwebRoundTripper_BlocksSSRFTargetBeforeCallingCobweb(t *testing.T) {
	called := false
	cobweb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer cobweb.Close()
	t.Setenv("COBWEB_ADDR", cobweb.URL)

	rt := &cobwebRoundTripper{}
	for _, target := range []string{
		"http://127.0.0.1/secret",
		"http://169.254.169.254/latest/meta-data/",
	} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if _, err := rt.RoundTrip(req); err == nil {
			t.Errorf("RoundTrip(%s) succeeded, want blocked", target)
		}
	}
	if called {
		t.Error("cobweb sidecar was called for an SSRF-blocked target")
	}
}

func TestCobwebRoundTripper_AllowsLegitTargetToCallCobweb(t *testing.T) {
	called := false
	cobweb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer cobweb.Close()
	t.Setenv("COBWEB_ADDR", cobweb.URL)

	rt := &cobwebRoundTripper{}
	req, _ := http.NewRequest(http.MethodGet, "https://cdn.example/seg/1.ts", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if !called {
		t.Error("cobweb sidecar was not called for a legitimate target")
	}
}

func TestCobwebRoundTripper_KeepsUAWithClearanceCookie(t *testing.T) {
	var gotBody map[string]any
	cobweb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(200)
	}))
	defer cobweb.Close()
	t.Setenv("COBWEB_ADDR", cobweb.URL)

	rt := &cobwebRoundTripper{}
	req, _ := http.NewRequest(http.MethodGet, "https://cdn.example/x", nil)
	req.Header.Set("User-Agent", "SessionUA/1.0")
	req.Header.Set("Cookie", "cf_clearance=xyz; other=1")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	hdrs, _ := gotBody["headers"].(map[string]any)
	if hdrs["User-Agent"] != "SessionUA/1.0" {
		t.Errorf("UA should be kept when cf_clearance present: %v", hdrs)
	}
}

// A UA pinned via the X-Force-User-Agent sentinel must survive the cobweb relay
// (which otherwise drops it), and the internal marker must not leak to cobweb.
func TestCobwebRoundTripper_KeepsPinnedUA(t *testing.T) {
	var gotBody map[string]any
	cobweb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(200)
	}))
	defer cobweb.Close()
	t.Setenv("COBWEB_ADDR", cobweb.URL)

	xhdr := b64url(`{"X-Force-User-Agent":"PinnedUA/1.0"}`)
	req, err := buildProxyRequest("GET", "https://cdn.example/a.m3u8", "https://player.example/", "", xhdr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&cobwebRoundTripper{}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	hdrs, _ := gotBody["headers"].(map[string]any)
	if hdrs["User-Agent"] != "PinnedUA/1.0" {
		t.Errorf("pinned UA lost in relay: %v", hdrs["User-Agent"])
	}
	if _, leaked := hdrs[pinUAHeader]; leaked {
		t.Errorf("internal marker leaked to cobweb: %v", hdrs)
	}
}

// With the sidecar's api_key configured, /v1/fetch must carry X-Api-Key like
// every other cobweb call — before, it didn't, so enabling the key broke all
// proxied streams.
func TestCobwebRoundTripper_SendsAPIKey(t *testing.T) {
	var gotKey string
	cobweb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(200)
	}))
	defer cobweb.Close()
	t.Setenv("COBWEB_ADDR", cobweb.URL)

	orig := managers.BrowserClient
	t.Cleanup(func() { managers.BrowserClient = orig })
	if err := managers.ConnectBrowserClient(cobweb.URL, "k3y"); err != nil {
		t.Fatal(err)
	}

	rt := &cobwebRoundTripper{}
	req, _ := http.NewRequest(http.MethodGet, "https://cdn.example/seg/1.ts", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if gotKey != "k3y" {
		t.Fatalf("X-Api-Key = %q, want k3y", gotKey)
	}
}
