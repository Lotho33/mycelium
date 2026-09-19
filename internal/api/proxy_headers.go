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

// setBrowserBaselineHeaders lays down a coherent modern-Chrome header set.
// Every upstream request starts from this, so it stays consistent even when the
// sniffer captured only a partial set. Some CDNs return an error to a request
// missing accept / sec-fetch-* / sec-ch-ua — which is what we ended up sending
// when xhdr carried nothing but a user-agent.
//
// accept-encoding is deliberately left unset: net/http adds "gzip" and
// transparently decodes the response only while we don't set the header
// ourselves — setting it here would hand the playlist/segment body back
// compressed.
func setBrowserBaselineHeaders(h http.Header) {
	h.Set("accept", "*/*")
	h.Set("accept-language", "en-US,en;q=0.9")
	// No "Connection" header: the upstream transport speaks HTTP/2, which does
	// not carry one. On the cobweb path the UA here is dropped and cobweb's own
	// wreq profile picks one that matches its fingerprint; on the standard path
	// buildProxyRequest forces this value (unless a cf_clearance cookie pins a
	// captured UA).
	h.Set("user-agent", compatUA)
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "cross-site")
}

// harmonizeClientHints rewrites the sec-ch-ua* trio so it always agrees with the
// final User-Agent. A Chrome-on-Linux UA carrying Windows/older-version hints
// (or the reverse) is internally inconsistent; deriving the hints from the UA
// keeps the two aligned whichever UA won the merge.
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

// applyFetchMetadata sets Referer/Origin and a Sec-Fetch-Site value consistent
// with the relationship between the target and the referer, the way a browser
// would. A media subresource fetched same-origin (a playlist requested from its
// own embed page) carries `Sec-Fetch-Site: same-origin` and NO `Origin` header;
// sending `cross-site` plus an explicit `Origin` there, as the old code always
// did, is inconsistent with real browser behaviour.
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

	// Chrome omits Origin on a same-origin GET/HEAD; it sends it for CORS
	// (cross-site / same-site) requests and for any non-GET.
	if site == "same-origin" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		req.Header.Del("origin")
	} else {
		req.Header.Set("origin", refURL.Scheme+"://"+refURL.Host)
	}
}

// sameRegistrableDomain is a PSL-free heuristic: it compares the last two dot
// labels. Good enough for the single-label TLDs common among stream CDNs; it
// would misjudge a multi-part TLD like co.uk.
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

// buildProxyRequest costruisce una richiesta HTTP verso il CDN upstream.
// Parte sempre da un baseline Chrome coerente (setBrowserBaselineHeaders), poi
// sovrascrive con gli header REALI catturati dal sniffer (xhdrRaw, base64url
// JSON) quando presenti: così un UA/x-*/authorization autentico vince, ma una
// cattura scarna (solo user-agent) mantiene comunque accept / sec-fetch-* /
// sec-ch-ua che alcuni CDN richiedono.
func buildProxyRequest(method, targetURL, origin, cookiesRaw, xhdrRaw string) (*http.Request, error) {
	req, err := http.NewRequest(method, targetURL, nil)
	if err != nil {
		return nil, err
	}

	setBrowserBaselineHeaders(req.Header)
	// Referer/Origin/Sec-Fetch-Site coerenti col rapporto target↔referer.
	// Prima dell'overlay xhdr, così un sec-fetch-site realmente catturato vince.
	applyFetchMetadata(req, origin)

	// pinnedUA: set only via the X-Force-User-Agent sentinel below — a plain
	// "User-Agent" in xhdr (e.g. one captured by mycelium.browser.sniff) is
	// deliberately NOT enough, see the UA policy comment below.
	pinnedUA := false
	if xhdrRaw != "" {
		if decoded := proxyB64Decode(xhdrRaw); decoded != "" {
			var hdrs map[string]string
			if jerr := json.Unmarshal([]byte(decoded), &hdrs); jerr == nil {
				for k, v := range hdrs {
					// net/http gestisce accept-encoding da sé (vedi setBrowserBaselineHeaders).
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

	// Decode cookies early — needed to decide the UA below.
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
	//   - normally force compatUA so the UA and its client-hints are internally
	//     consistent (on the cobweb path this UA is then dropped and cobweb
	//     supplies one matching its own fingerprint; on the standard path it's
	//     what goes on the wire).
	//   - BUT when a session-verification cookie (cf_clearance) is present, it
	//     was issued to the browser that solved the check, bound to THAT
	//     user-agent; re-presenting it with a different UA gets it rejected and
	//     the check re-served. So keep the captured (session) UA.
	//   - SAME reasoning when resolve_stream sets the X-Force-User-Agent
	//     sentinel (pinnedUA, above): some upstreams bind a stream token to
	//     the exact UA that requested it — not only via a Cloudflare-style
	//     cookie — and overwriting it here breaks that token, a real case
	//     found live (2026-09-19, a Telegram mini-app backend issuing
	//     UA-bound HLS tokens). This is a SEPARATE, explicit signal from a
	//     plain "User-Agent" in xhdr: a mycelium.browser.sniff capture also
	//     carries one, and TestBuildProxyRequest_ForcesProfileUA exists
	//     precisely because that captured UA must NOT be trusted (it can name
	//     a newer browser build than compatUA's fingerprint, which is
	//     internally inconsistent) — only a plugin that deliberately asks for
	//     the sentinel is presumed to mean it.
	if !strings.Contains(cookieStr, "cf_clearance") && !pinnedUA {
		req.Header.Set("user-agent", compatUA)
	}
	if pinnedUA {
		// Consumed (and stripped) by the upstream transports: the cobweb relay
		// otherwise drops the UA so its own profile supplies one, which would
		// undo the pin.
		req.Header.Set(pinUAHeader, "1")
	}

	// Tiene i client-hint coerenti con lo UA finale.
	harmonizeClientHints(req.Header)

	if cookieStr != "" {
		req.Header.Set("cookie", cookieStr)
	}
	return req, nil
}

// buildXhdrSuffix codifica gli header extra (tutti tranne Referer/Cookie/Origin) come
// parametro URL &xhdr=<b64url(json)>. Restituisce stringa vuota se non ci sono header extra.
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

// canonicalHeaderKeys returns h with every key run through
// http.CanonicalHeaderKey ("cookie" → "Cookie"). On a case-collision a
// non-empty value wins over an empty one. Returns nil for a nil map. cobweb's
// sniff hands header names back lowercased; the proxy looks them up title-cased.
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
