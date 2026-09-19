package api

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func b64url(v string) string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(v))
}

// The sniffed UA is NOT forwarded upstream: it may name a newer browser build
// than the browser profile pins (compatProfile), and a UA that disagrees with
// the fingerprint is internally inconsistent. buildProxyRequest forces compatUA
// after the xhdr overlay; the client hints then follow that one build. The rest
// of the captured context (referer, sec-fetch-*) still survives.
func TestBuildProxyRequest_ForcesProfileUA(t *testing.T) {
	xhdr := b64url(`{"user-agent":"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"}`)

	req, err := buildProxyRequest("GET", "https://stream.example/playlist/1", "https://stream.example/embed/1", "", xhdr)
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}

	if got := req.Header.Get("user-agent"); got != compatUA {
		t.Errorf("UA not pinned to the profile: %q", got)
	}
	for _, h := range []string{"accept", "accept-language", "sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "sec-ch-ua"} {
		if req.Header.Get(h) == "" {
			t.Errorf("baseline header %q missing after thin-xhdr merge", h)
		}
	}
	if got := req.Header.Get("sec-ch-ua-platform"); got != `"Linux"` {
		t.Errorf("sec-ch-ua-platform = %q, want \"Linux\" (compatUA is a Linux UA)", got)
	}
	major := reChromeMajor.FindStringSubmatch(compatUA)[1]
	if got := req.Header.Get("sec-ch-ua"); !strings.Contains(got, `v="`+major+`"`) {
		t.Errorf("sec-ch-ua = %q, want major %s to match the forced UA", got, major)
	}
	if req.Header.Get("referer") != "https://stream.example/embed/1" {
		t.Errorf("referer = %q", req.Header.Get("referer"))
	}
	// playlist and embed share the origin → same-origin and NO Origin header.
	if got := req.Header.Get("sec-fetch-site"); got != "same-origin" {
		t.Errorf("sec-fetch-site = %q, want same-origin (playlist requested from same-origin embed)", got)
	}
	if got := req.Header.Get("origin"); got != "" {
		t.Errorf("origin = %q, want empty on a same-origin GET", got)
	}
	if req.Header.Get("accept-encoding") != "" {
		t.Errorf("accept-encoding must stay unset so net/http decodes gzip: %q", req.Header.Get("accept-encoding"))
	}
}

// When a cf_clearance cookie is present the captured (session) UA is kept, not
// overridden — that cookie was issued to the browser that completed the
// verification, bound to its UA, and a different UA gets it rejected.
func TestBuildProxyRequest_KeepsUAWithCfClearance(t *testing.T) {
	const sessUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
	xhdr := b64url(`{"user-agent":"` + sessUA + `"}`)
	cookies := b64url("cf_clearance=abc123-deadbeef; other=1")

	req, err := buildProxyRequest("GET", "https://stream.example/playlist/1", "https://stream.example/tv/1/1/1", cookies, xhdr)
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}
	if got := req.Header.Get("user-agent"); got != sessUA {
		t.Errorf("UA = %q, want the session UA kept (cf_clearance present)", got)
	}
	if got := req.Header.Get("sec-ch-ua"); !strings.Contains(got, `v="151"`) {
		t.Errorf("sec-ch-ua = %q, want v=\"151\" to match the kept UA", got)
	}
	if got := req.Header.Get("cookie"); !strings.Contains(got, "cf_clearance=abc123-deadbeef") {
		t.Errorf("cookie not forwarded: %q", got)
	}
}

// A plain "user-agent" in xhdr (e.g. a mycelium.browser.sniff capture) is NOT
// enough to skip the compatUA override — see
// TestBuildProxyRequest_ForcesProfileUA. Only the explicit
// X-Force-User-Agent sentinel does, for a plugin's resolve_stream that
// deliberately needs its exact UA preserved (e.g. an upstream binding a
// stream token to it) without a Cloudflare-style cookie in play.
func TestBuildProxyRequest_KeepsUAWithForceSentinel(t *testing.T) {
	const pinnedUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36"
	xhdr := b64url(`{"X-Force-User-Agent":"` + pinnedUA + `"}`)

	req, err := buildProxyRequest("GET", "https://stream.example/playlist/1", "https://stream.example/embed/1", "", xhdr)
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}
	if got := req.Header.Get("user-agent"); got != pinnedUA {
		t.Errorf("UA = %q, want the pinned UA kept (X-Force-User-Agent present)", got)
	}
	if req.Header.Get("x-force-user-agent") != "" {
		t.Errorf("x-force-user-agent sentinel leaked into the outgoing request headers")
	}
	if got := req.Header.Get("sec-ch-ua-platform"); got != `"Windows"` {
		t.Errorf("sec-ch-ua-platform = %q, want \"Windows\" to follow the pinned UA", got)
	}
}

// A cross-site fetch (segment host differs from the embed's registrable domain)
// keeps Sec-Fetch-Site: cross-site and an explicit Origin.
func TestBuildProxyRequest_CrossSiteKeepsOrigin(t *testing.T) {
	req, err := buildProxyRequest("GET", "https://seg.othercdn.example/s/1.ts", "https://stream.example/embed/1", "", "")
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}
	if got := req.Header.Get("sec-fetch-site"); got != "cross-site" {
		t.Errorf("sec-fetch-site = %q, want cross-site", got)
	}
	if got := req.Header.Get("origin"); got != "https://stream.example" {
		t.Errorf("origin = %q, want https://stream.example on a cross-site request", got)
	}
}

// A subdomain of the same registrable domain is same-site, not cross-site.
func TestBuildProxyRequest_SameSiteSubdomain(t *testing.T) {
	req, err := buildProxyRequest("GET", "https://cdn.stream.example/s/1.ts", "https://stream.example/embed/1", "", "")
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}
	if got := req.Header.Get("sec-fetch-site"); got != "same-site" {
		t.Errorf("sec-fetch-site = %q, want same-site", got)
	}
}

// With no xhdr at all the request still carries the coherent baseline.
func TestBuildProxyRequest_NoXhdrBaseline(t *testing.T) {
	req, err := buildProxyRequest("GET", "https://cdn.example/seg.ts", "https://cdn.example/embed", "", "")
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}
	if !strings.Contains(req.Header.Get("user-agent"), "Chrome/") {
		t.Errorf("baseline UA missing: %q", req.Header.Get("user-agent"))
	}
	ua := req.Header.Get("user-agent")
	m := reChromeMajor.FindStringSubmatch(ua)
	if m == nil || !strings.Contains(req.Header.Get("sec-ch-ua"), `v="`+m[1]+`"`) {
		t.Errorf("sec-ch-ua %q not consistent with UA %q", req.Header.Get("sec-ch-ua"), ua)
	}
}

// A caller passing explicit accept-encoding via xhdr must not leak into the
// request (net/http owns it).
func TestBuildProxyRequest_DropsAcceptEncoding(t *testing.T) {
	hdrs := map[string]string{"user-agent": "x", "accept-encoding": "br"}
	j, _ := json.Marshal(hdrs)
	req, err := buildProxyRequest("GET", "https://cdn.example/x", "", "", b64url(string(j)))
	if err != nil {
		t.Fatalf("buildProxyRequest: %v", err)
	}
	if req.Header.Get("accept-encoding") != "" {
		t.Errorf("accept-encoding leaked from xhdr: %q", req.Header.Get("accept-encoding"))
	}
}
