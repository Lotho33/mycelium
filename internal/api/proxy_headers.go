package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var reChromeMajor = regexp.MustCompile(`Chrome/(\d+)`)

func proxyB64Decode(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, " ", "+")
	decoded, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(s)
	if err != nil {
		if m := len(s) % 4; m != 0 {
			s += strings.Repeat("=", 4-m)
		}
		decoded, _ = base64.StdEncoding.DecodeString(s)
	}
	return string(decoded)
}

func proxyURLJoin(base, ref string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return ref
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return baseURL.ResolveReference(refURL).String()
}

// setBrowserBaselineHeaders lays down a coherent browser header set every
// upstream request starts from, so it stays complete when the sniffer
// captured only part of it (some CDNs reject a request without accept /
// sec-fetch-* / sec-ch-ua). accept-encoding stays unset: net/http then adds
// gzip and decodes transparently.
func setBrowserBaselineHeaders(h http.Header) {
	h.Set("accept", "*/*")
	h.Set("accept-language", "en-US,en;q=0.9")
	// No "Connection" header (HTTP/2). On the relay path the UA is replaced by
	// the browser service's; on the direct path buildProxyRequest decides it.
	h.Set("user-agent", compatUA)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "cross-site")
}

// harmonizeClientHints rewrites the sec-ch-ua* headers to agree with the
// final User-Agent.
func harmonizeClientHints(h http.Header) {
	ua := h.Get("user-agent")
	if ua == "" {
		return
	}

	platform := `"Windows"`
	switch {
	case strings.Contains(ua, "Android"):
		platform = `"Android"`
	case strings.Contains(ua, "Linux"), strings.Contains(ua, "X11"):
		platform = `"Linux"`
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		platform = `"macOS"`
	}
	h.Set("sec-ch-ua-platform", platform)

	mobile := "?0"
	if strings.Contains(ua, "Mobile") || strings.Contains(ua, "Android") {
		mobile = "?1"
	}
	h.Set("sec-ch-ua-mobile", mobile)

	major := "131"
	if m := reChromeMajor.FindStringSubmatch(ua); m != nil {
		major = m[1]
	}
	h.Set("sec-ch-ua", fmt.Sprintf(`"Google Chrome";v="%s", "Chromium";v="%s", "Not_A Brand";v="24"`, major, major))
}

// applyFetchMetadata sets Referer/Origin and Sec-Fetch-Site the way a browser
// would for the target/referer relationship (same-origin media fetches carry
// no Origin).
func applyFetchMetadata(req *http.Request, referer string) {
	if referer == "" {
		req.Header.Set("sec-fetch-site", "none")
		req.Header.Del("origin")
		return
	}
	req.Header.Set("referer", referer)

	refURL, err := url.Parse(referer)
	if err != nil || refURL.Host == "" {
		return
	}

	site := "cross-site"
	switch {
	case strings.EqualFold(refURL.Host, req.URL.Host) && strings.EqualFold(refURL.Scheme, req.URL.Scheme):
		site = "same-origin"
	case sameRegistrableDomain(refURL.Hostname(), req.URL.Hostname()):
		site = "same-site"
	}
	req.Header.Set("sec-fetch-site", site)

	// A browser omits Origin on a same-origin GET/HEAD.
	if site == "same-origin" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		req.Header.Del("origin")
	} else {
		req.Header.Set("origin", refURL.Scheme+"://"+refURL.Host)
	}
}

// sameRegistrableDomain compares the last two labels (no public-suffix list:
// it misjudges multi-part TLDs like co.uk).
func sameRegistrableDomain(a, b string) bool {
	last2 := func(h string) string {
		p := strings.Split(strings.ToLower(strings.TrimSuffix(h, ".")), ".")
		if len(p) < 2 {
			return strings.ToLower(h)
		}
		return p[len(p)-2] + "." + p[len(p)-1]
	}
	x := last2(a)
	return x != "" && x == last2(b)
}

// pinUAHeader is an internal marker (never sent upstream) telling the transport
// to keep the request's User-Agent instead of substituting its own profile.
const pinUAHeader = "X-Mycelium-Pin-UA"

// buildProxyRequest builds an upstream request: a coherent browser baseline
// (setBrowserBaselineHeaders), overlaid with the headers captured for the
// stream (xhdrRaw, base64url JSON) when present.
func buildProxyRequest(method, targetURL, origin, cookiesRaw, xhdrRaw string) (*http.Request, error) {
	req, err := http.NewRequest(method, targetURL, nil)
	if err != nil {
		return nil, err
	}

	setBrowserBaselineHeaders(req.Header)
	// Referer/Origin/Sec-Fetch-Site before the xhdr overlay, so captured values
	// win.
	applyFetchMetadata(req, origin)

	// pinnedUA: set only through the X-Force-User-Agent sentinel; a plain
	// "User-Agent" in xhdr is not enough (see the UA policy below).
	pinnedUA := false
	if xhdrRaw != "" {
		if decoded := proxyB64Decode(xhdrRaw); decoded != "" {
			var hdrs map[string]string
			if jerr := json.Unmarshal([]byte(decoded), &hdrs); jerr == nil {
				for k, v := range hdrs {
					// net/http handles accept-encoding (see setBrowserBaselineHeaders).
					if strings.EqualFold(k, "accept-encoding") {
						continue
					}
					if strings.EqualFold(k, "x-force-user-agent") {
						req.Header.Set("user-agent", v)
						pinnedUA = true
						continue
					}
					req.Header.Set(k, v)
				}
			}
		}
	}

	// Decode cookies early: needed to decide the UA.
	var cookieStr string
	if cookiesRaw != "" {
		if decoded := proxyB64Decode(cookiesRaw); decoded != "" {
			if c, uerr := url.QueryUnescape(decoded); uerr == nil {
				cookieStr = c
			} else {
				cookieStr = decoded
			}
		}
	}

	// UA policy:
	//   - normally force compatUA, so UA and client hints agree;
	//   - keep the captured UA when a session-verification cookie
	//     (cf_clearance) is present: it is bound to the UA it was issued to;
	//   - keep it when resolve_stream sets the X-Force-User-Agent sentinel:
	//     some upstreams bind their stream token to the exact UA. A sniffed UA
	//     alone is not trusted (it can be inconsistent with compatUA's client
	//     hints).
	if !strings.Contains(cookieStr, "cf_clearance") && !pinnedUA {
		req.Header.Set("user-agent", compatUA)
	}
	if pinnedUA {
		// Read and stripped by the transports: the relay would otherwise replace
		// the UA and undo the pin.
		req.Header.Set(pinUAHeader, "1")
	}

	// Keep client hints consistent with the final UA.
	harmonizeClientHints(req.Header)

	if cookieStr != "" {
		req.Header.Set("cookie", cookieStr)
	}
	return req, nil
}

// buildXhdrSuffix encodes the extra headers (all but Referer/Cookie/Origin)
// as &xhdr=<b64url(json)>, "" when there are none.
func buildXhdrSuffix(b64enc func(string) string, headers map[string]string) string {
	skip := map[string]bool{
		"Referer": true, "referer": true,
		"Cookie": true, "cookie": true,
		"Origin": true, "origin": true,
	}
	extra := make(map[string]string)
	for k, v := range headers {
		if !skip[k] {
			extra[k] = v
		}
	}
	if len(extra) == 0 {
		return ""
	}
	j, err := json.Marshal(extra)
	if err != nil {
		return ""
	}
	return "&xhdr=" + url.QueryEscape(b64enc(string(j)))
}

// canonicalHeaderKeys returns h with canonical keys ("cookie" → "Cookie").
// On a collision a non-empty value wins. nil for a nil map.
func canonicalHeaderKeys(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		ck := http.CanonicalHeaderKey(k)
		if ex, ok := out[ck]; ok && ex != "" && v == "" {
			continue
		}
		out[ck] = v
	}
	return out
}
