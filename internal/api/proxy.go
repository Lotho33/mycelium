package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

var (
	upstreamMu   sync.Mutex
	proxySession *http.Client
	// Client dedicato ai segmenti TS: timeout più alto, connessioni persistenti
	segmentClient *http.Client
)

// proxyDebug gates the verbose proxy log lines that dump full request/response
// headers and whole playlist bodies — those carry upstream Cookie/Authorization
// and tokenised segment URLs and must not land in data/logs/*.log by default.
var proxyDebug = os.Getenv("MYCELIUM_DEBUG") == "1"

// proxyAllowUnsigned disables the /proxy/* URL signature check. Escape hatch for
// debugging only — with it set the HLS proxy is an open relay again.
var proxyAllowUnsigned = os.Getenv("MYCELIUM_PROXY_ALLOW_UNSIGNED") == "1"

// requireProxySig rejects a /proxy/* request whose query carries no valid HMAC
// (minted by media_handler.go / the playlist rewriter). Returns true if the
// caller should stop. No-op when MYCELIUM_PROXY_ALLOW_UNSIGNED=1 or when no
// signing key is configured (keeps a keyless dev boot working).
func requireProxySig(w http.ResponseWriter, r *http.Request) bool {
	if proxyAllowUnsigned || !core.ProxySignEnabled() {
		return false
	}
	if core.VerifyProxyURL(r.URL.Query()) {
		return false
	}
	log.Printf("[proxy] rejected unsigned/tampered request from %s %s", realIP(r), logURL(r.URL.String()))
	http.Error(w, "bad or missing signature", http.StatusForbidden)
	return true
}

// logURL trims a URL for logging: scheme://host/path only, the query replaced
// with a marker. Both the incoming /proxy/* URLs (base64'd upstream URL +
// cookies in the query) and the decoded upstream URLs (CDN auth tokens in the
// query) would otherwise leak secrets into data/logs/*.log. Full URLs are
// logged only under MYCELIUM_DEBUG=1 (proxyDebug).
func logURL(raw string) string {
	if proxyDebug {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	s := u.Path
	if u.Scheme != "" || u.Host != "" {
		// absolute URL (an upstream target); a bare incoming request URL is
		// path-only and stays that way — no "://" prefix.
		s = u.Scheme + "://" + u.Host + u.Path
	}
	if u.RawQuery != "" {
		s += "?<redacted>"
	}
	return s
}

// resetUpstreamClients drops the cached HLS-proxy upstream clients (direct and
// VPN-routed) so the next request rebuilds them. Called when a setting that
// changes their transport — e.g. http_profile — is saved from the dashboard,
// so the change takes effect without a restart.
func resetUpstreamClients() {
	upstreamMu.Lock()
	proxySession = nil
	segmentClient = nil
	upstreamMu.Unlock()

	vpnMu.Lock()
	vpnSession = nil
	vpnSegment = nil
	vpnAddr = ""
	vpnMu.Unlock()
}

const proxyUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var reKeyURI = regexp.MustCompile(`URI=["']([^"']+)["']`)
var reChromeMajor = regexp.MustCompile(`Chrome/(\d+)`)

// getProxySession returns the process-wide client for the small upstream
// fetches: HLS master/media playlists and AES keys. Stable for the whole
// process (rebuilt only by resetUpstreamClients), exactly like
// getSegmentClient below.
//
// It used to rebuild itself (new client + NEW empty cookie jar + new
// transport) every 40 calls. On a live stream — a playlist reload every few
// seconds plus key fetches — that fired every ~3-4 minutes and wiped any
// cookie the upstream CDN had set on the session, so playback stalled a few
// minutes in on cookie-gated CDNs. The transport manages its own connection
// pool health, so there was nothing for the periodic rebuild to fix.
func getProxySession() *http.Client {
	upstreamMu.Lock()
	defer upstreamMu.Unlock()
	if proxySession == nil {
		jar, _ := cookiejar.New(nil)
		proxySession = &http.Client{
			Timeout: 15 * time.Second,
			Jar:     jar,
			Transport: newUTLSTransport(utlsTransportOpts{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 10,
			}),
		}
	}
	return proxySession
}

func getSegmentClient() *http.Client {
	upstreamMu.Lock()
	defer upstreamMu.Unlock()
	if segmentClient == nil {
		jar, _ := cookiejar.New(nil)
		segmentClient = &http.Client{
			Timeout: 60 * time.Second,
			Jar:     jar,
			Transport: newUTLSTransport(utlsTransportOpts{
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				IdleConnTimeout:       120 * time.Second,
				ResponseHeaderTimeout: 15 * time.Second,
			}),
		}
	}
	return segmentClient
}

// VPN-routed variants of the proxy clients, used for streams from plugins that
// are VPN-routed (every plugin except direct_egress ones). They are rebuilt
// when the VPN proxy address changes.
var (
	vpnSession *http.Client
	vpnSegment *http.Client
	vpnAddr    string
	vpnMu      sync.Mutex
)

// vpnClients returns the (playlist/key, segment) clients routed through proxyURL.
func vpnClients(proxyURL string) (*http.Client, *http.Client) {
	vpnMu.Lock()
	defer vpnMu.Unlock()
	if vpnSession == nil || vpnAddr != proxyURL {
		jar1, _ := cookiejar.New(nil)
		vpnSession = &http.Client{Timeout: 15 * time.Second, Jar: jar1, Transport: newUTLSProxyTransport(proxyURL, utlsTransportOpts{
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 10,
		})}

		jar2, _ := cookiejar.New(nil)
		vpnSegment = &http.Client{Timeout: 60 * time.Second, Jar: jar2, Transport: newUTLSProxyTransport(proxyURL, utlsTransportOpts{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       120 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		})}

		vpnAddr = proxyURL
	}
	return vpnSession, vpnSegment
}

// egressProxyAddr maps the &egr=<name> carried on a proxied playlist/segment
// URL to the proxy address its traffic must take. An empty name is a URL minted
// before per-egress threading (or a generic caller): fall back to the legacy
// single proxy. A named-but-disabled/unknown egress is an error so the fetch
// fails closed rather than leaking the box's real IP.
func egressProxyAddr(egr string) (string, error) {
	egr = strings.TrimSpace(egr)
	if egr == "" {
		addr := engine.LuaPlugins.GetProxyAddr()
		if addr == "" {
			return "", fmt.Errorf("VPN required but no proxy configured")
		}
		return addr, nil
	}
	u, ok := managers.ResolveEgressProxy(egr)
	if !ok || u == "" {
		return "", fmt.Errorf("egress %q non disponibile", egr)
	}
	return u, nil
}

// upstreamPlaylistClient / upstreamSegmentClient pick the direct or VPN client.
// When useVPN is set but the selected egress isn't usable they return an error
// so the fetch fails closed rather than leaking the plugin's real egress IP.
func upstreamPlaylistClient(useVPN bool, egr string) (*http.Client, error) {
	if !useVPN {
		return getProxySession(), nil
	}
	addr, err := egressProxyAddr(egr)
	if err != nil {
		return nil, err
	}
	s, _ := vpnClients(addr)
	return s, nil
}

func upstreamSegmentClient(useVPN bool, egr string) (*http.Client, error) {
	if !useVPN {
		return getSegmentClient(), nil
	}
	addr, err := egressProxyAddr(egr)
	if err != nil {
		return nil, err
	}
	_, seg := vpnClients(addr)
	return seg, nil
}

// retryBackoff returns the delay before retry attempt N+1 (150ms, then
// 300ms) — small enough to not add noticeable latency to a player stall, but
// enough to not hammer a CDN edge that's already struggling.
func retryBackoff(attempt int) time.Duration {
	return time.Duration(attempt) * 150 * time.Millisecond
}

// ─── segment fetch pacing ────────────────────────────────────────────────────
// A player opens many parallel connections at stream start and asks for a
// burst of segments to fill its buffer. Some segment CDNs (vix-content.net)
// tolerate only a small burst per IP, then 403 every further request for a
// while. segAcquire caps how many upstream segment fetches run at once PER CDN
// HOST — the player's excess requests just wait here, which is invisible since
// it's buffering ahead. This is the actual fix for "first few .ts arrive, then
// a wall of 403".
var (
	segGateMu sync.Mutex
	segGates  = map[string]chan struct{}{}
)

func segConcurrency() int {
	if v := strings.TrimSpace(os.Getenv("MYCELIUM_SEGMENT_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

func segAcquire(ctx context.Context, host string) (func(), error) {
	if host == "" {
		return func() {}, nil
	}
	segGateMu.Lock()
	g := segGates[host]
	if g == nil {
		g = make(chan struct{}, segConcurrency())
		segGates[host] = g
	}
	segGateMu.Unlock()
	select {
	case g <- struct{}{}:
		return func() { <-g }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// noteChallengeIfAny records that an upstream responded with an interactive
// verification page (advertised via the `Cf-Mitigated: challenge` response
// header) so the dashboard/Pileus can tell the operator it needs solving in a
// browser. domain = registrable domain of rawURL; egressLabel is the egress
// profile name (or "direct") — a hint for the dashboard, not a key.
func noteChallengeIfAny(resp *http.Response, rawURL string, egressLabel string) {
	if resp == nil || !strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge") {
		return
	}
	dom := ""
	if u, err := url.Parse(rawURL); err == nil {
		dom = registrableHost(u.Hostname())
	}
	if dom == "" {
		return
	}
	if egressLabel == "" {
		egressLabel = "direct"
	}
	log.Printf("[proxy/verify] %s (egress %s) → upstream wants interactive verification", dom, egressLabel)
	managers.RecordChallenge(dom, egressLabel)
}

// videoEgressSuffix returns the query fragment ("&vpn=1&egr=<name>") that pins
// every proxied playlist/segment/key URL of pluginID to the egress profile the
// operator picked for it. Empty when the plugin's video flow goes direct.
func videoEgressSuffix(pluginID string) string {
	name := engine.LuaPlugins.PluginEgress(pluginID)
	if name == "" || name == managers.EgressDirect {
		return ""
	}
	return "&vpn=1&egr=" + url.QueryEscape(name)
}

// childVPNSuffix rebuilds the "&vpn=1[&egr=<name>]" fragment for the URLs a
// playlist rewrite emits, carrying the egress name from the incoming request.
func childVPNSuffix(q url.Values) string {
	if q.Get("vpn") != "1" {
		return ""
	}
	s := "&vpn=1"
	if egr := q.Get("egr"); egr != "" {
		s += "&egr=" + url.QueryEscape(egr)
	}
	return s
}

// challengeEgressLabel is the human hint recorded with an interactive-verification
// hit: the egress profile name when known, else "vpn"/"direct".
func challengeEgressLabel(useVPN bool, egr string) string {
	if egr != "" {
		return egr
	}
	if useVPN {
		return "vpn"
	}
	return "direct"
}

// registrableHost: last two dot labels (PSL-free) — fine for the single-label
// TLDs common among stream CDNs.
func registrableHost(host string) string {
	p := strings.Split(strings.ToLower(strings.Trim(host, ".")), ".")
	if len(p) < 2 {
		return strings.ToLower(host)
	}
	return p[len(p)-2] + "." + p[len(p)-1]
}

// segRetryBackoff is the wait before retrying a segment: a plain 403/429 is
// rate-limiting, not a transient glitch — wait seconds, not milliseconds.
func segRetryBackoff(attempt, status int) time.Duration {
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		d := time.Duration(attempt) * time.Second
		if d > 4*time.Second {
			d = 4 * time.Second
		}
		return d
	}
	return retryBackoff(attempt)
}

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

	if xhdrRaw != "" {
		if decoded := proxyB64Decode(xhdrRaw); decoded != "" {
			var hdrs map[string]string
			if jerr := json.Unmarshal([]byte(decoded), &hdrs); jerr == nil {
				for k, v := range hdrs {
					// net/http gestisce accept-encoding da sé (vedi setBrowserBaselineHeaders).
					if strings.EqualFold(k, "accept-encoding") {
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
	if !strings.Contains(cookieStr, "cf_clearance") {
		req.Header.Set("user-agent", compatUA)
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

func proxyResolveForPlaylist(r *http.Request, sidRaw, uidRaw string) (newPlaylistURL, newOrigin string) {
	decoded := proxyB64Decode(sidRaw)
	if decoded == "" {
		return "", ""
	}
	idx := strings.IndexByte(decoded, 0)
	if idx < 0 {
		return "", ""
	}
	pluginID := decoded[:idx]
	sourceID := decoded[idx+1:]

	var resolvedURL string
	var resolvedHeaders map[string]string

	if engine.LuaPlugins.Has(pluginID) {
		// Lua plugins live in a separate registry from the native/gRPC ones
		// resolve through the Lua entrypoint directly,
		// same call lua_pipeline.go's ResolveStream makes.
		// force_refresh tells plugins that cache their resolved URL to skip the
		// cache read and resolve a fresh CDN token instead of handing back the
		// same one that just got rejected.
		b, err := engine.LuaPlugins.CallEntrypointJSON(pluginID, engine.EPResolveStream, map[string]any{
			"stream_id":     sourceID,
			"force_refresh": true,
		}, "")
		if err != nil {
			log.Printf("[proxy/playlist] re-resolve %s/%s failed: %v", pluginID, sourceID, err)
			return "", ""
		}
		var result struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(b, &result); err != nil || result.URL == "" {
			log.Printf("[proxy/playlist] re-resolve %s/%s: invalid lua result: %v", pluginID, sourceID, err)
			return "", ""
		}
		resolvedURL, resolvedHeaders = result.URL, result.Headers
	} else {
		log.Printf("[proxy/playlist] re-resolve: plugin %q not found", pluginID)
		return "", ""
	}

	// cobweb lowercases sniffed header names; the lookups below and buildXhdrSuffix
	// are title-cased. Canonicalise so a captured Cookie survives the re-resolve.
	resolvedHeaders = canonicalHeaderKeys(resolvedHeaders)

	origin := resolvedHeaders["Referer"]
	if origin == "" {
		origin = resolvedHeaders["Origin"]
	}
	b64enc := func(v string) string {
		return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(v))
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	newSid := b64enc(pluginID + "\x00" + sourceID)
	uidParam := ""
	if uidRaw != "" {
		uidParam = "&uid=" + url.QueryEscape(uidRaw)
	}
	vpnSuffix := videoEgressSuffix(pluginID)
	xhdrSuffix := buildXhdrSuffix(b64enc, resolvedHeaders)
	// rr=1 marca questo URL come già ri-risolto: se anche il fetch successivo
	// fallisce, ProxyPlaylist restituisce 502 invece di ri-risolvere di nuovo,
	// evitando un loop di redirect 302 (token nuovo, stesso 403).
	u := fmt.Sprintf("%s://%s/proxy/playlist.m3u8?data=%s&origin=%s&cookies=%s&sid=%s%s%s%s&rr=1",
		scheme, r.Host,
		b64enc(resolvedURL),
		b64enc(origin),
		b64enc(resolvedHeaders["Cookie"]),
		url.QueryEscape(newSid),
		uidParam,
		xhdrSuffix,
		vpnSuffix,
	)
	return u, origin
}

// ProxyPlaylist godoc
//
//	@Summary		Proxy playlist HLS
//	@Description	Scarica la playlist M3U8 upstream e riscrive i segmenti/chiavi per passare dal proxy locale. Senza autenticazione — chiamato direttamente dai media player.
//	@Tags			Proxy HLS
//	@Param			data	query	string	true	"URL playlist upstream codificato in base64url"
//	@Param			origin	query	string	false	"Origin upstream codificato in base64url"
//	@Param			cookies	query	string	false	"Cookie upstream codificati in base64url"
//	@Produce		application/vnd.apple.mpegurl
//	@Success		200	{string}	string	"playlist M3U8 riscritta"
//	@Failure		502	{string}	string	"upstream error"
//	@Router			/proxy/playlist.m3u8 [get]
func ProxyPlaylist(w http.ResponseWriter, r *http.Request) {
	if requireProxySig(w, r) {
		return
	}
	q := r.URL.Query()
	dataRaw := q.Get("data")
	originRaw := q.Get("origin")
	cookiesRaw := q.Get("cookies")
	xhdrRaw := q.Get("xhdr")
	sidRaw := q.Get("sid")
	uidRaw := q.Get("uid") // user session identifier for tracking
	useVPN := q.Get("vpn") == "1"
	egr := q.Get("egr") // egress profile name (see videoEgressSuffix)

	realURL := proxyB64Decode(dataRaw)
	realOrigin := proxyB64Decode(originRaw)

	log.Printf("[proxy/playlist] url=%s origin=%s uid=%s xhdr_present=%v vpn=%v egr=%s", logURL(realURL), realOrigin, uidRaw, xhdrRaw != "", useVPN, egr)

	plClient, clErr := upstreamPlaylistClient(useVPN, egr)
	if clErr != nil {
		log.Printf("[proxy/playlist] %v url=%s", clErr, logURL(realURL))
		http.Error(w, clErr.Error(), http.StatusBadGateway)
		return
	}

	// Warm-cache hit: the pre-buffer step (prebuffer.go) already fetched this
	// exact upstream playlist during ResolveStream. Serve those bytes and skip
	// the round-trip — the rewrite below still runs, so a master playlist is
	// still rewritten to proxy variant URLs.
	var content []byte
	var fetchErr error
	if warm, ok := managers.Segments.Get(realURL); ok {
		content = warm
		// Serve the resolve-time copy once (fast first frame), then drop it:
		// a live media playlist's segment window slides, so mpv's next
		// reload must fetch a fresh copy from upstream. Keeping the frozen
		// copy for the whole 90s TTL is why live streams "stop transmitting
		// after a while" — mpv replays the same few segments and then runs
		// dry with no #EXT-X-ENDLIST to tell it the stream really ended.
		managers.Segments.Delete(realURL)
		log.Printf("[proxy/playlist] warm-cache hit (%d bytes), evicted for reload url=%s", len(warm), logURL(realURL))
	} else {
		// Deduplicate concurrent upstream fetches for the same URL.
		// All callers share one HTTP request; useful for live streams with multiple viewers.
		var shared bool
		content, fetchErr, shared = playlistInflight.Do(realURL, func() ([]byte, error) {
			req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, cookiesRaw, xhdrRaw)
			if err != nil {
				return nil, err
			}
			if proxyDebug {
				log.Printf("[proxy/playlist] sending headers=%v", req.Header)
			}
			// Detached from r.Context() on purpose: playlistInflight shares
			// this one upstream fetch across every concurrent caller, and
			// mpv opens a fresh connection for every HLS playlist reload
			// then closes it as soon as it has the body. If this fetch rode
			// the leader request's context, that leader closing its
			// connection (or any single client going away) would cancel the
			// fetch the other callers — and mpv's very next reload — are
			// blocked on, which surfaced as "Failed to reload playlist 0" /
			// "parse_playlist error Invalid data found" a few seconds into
			// every live stream.
			fetchCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			req = req.WithContext(fetchCtx)
			resp, err := plClient.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				bodyStr := strings.TrimSpace(string(body))
				if len(bodyStr) > 200 {
					bodyStr = bodyStr[:200]
				}
				log.Printf("[proxy/playlist] upstream %d body=%q", resp.StatusCode, bodyStr)
				if proxyDebug {
					log.Printf("[proxy/playlist] upstream %d headers=%v", resp.StatusCode, resp.Header)
				}
				noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
				return nil, fmt.Errorf("upstream %d", resp.StatusCode)
			}
			return io.ReadAll(resp.Body)
		})
		if shared {
			log.Printf("[proxy/playlist] shared fetch for %s", logURL(realURL))
		}
	}

	if fetchErr == nil && content != nil {
		segs := strings.Count(string(content), "#EXTINF")
		if proxyDebug {
			log.Printf("[proxy/playlist] fetched %d bytes, %d segments:\n%s", len(content), segs, string(content))
		} else {
			log.Printf("[proxy/playlist] fetched %d bytes, %d segments", len(content), segs)
		}
	}

	if fetchErr != nil {
		log.Printf("[proxy/playlist] fetch error: %v", fetchErr)
		if sidRaw != "" && q.Get("rr") == "" {
			if newURL, _ := proxyResolveForPlaylist(r, sidRaw, uidRaw); newURL != "" {
				log.Printf("[proxy/playlist] re-resolve → %s", newURL)
				http.Redirect(w, r, newURL, http.StatusFound)
				return
			}
		}
		http.Error(w, fetchErr.Error(), http.StatusBadGateway)
		return
	}

	playlistBase := realURL[:strings.LastIndex(realURL, "/")+1]
	lines := strings.Split(string(content), "\n")
	b64enc := func(s string) string {
		return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	proxyBase := fmt.Sprintf("%s://%s/proxy", scheme, r.Host)

	uidSuffix := ""
	if uidRaw != "" {
		uidSuffix = "&uid=" + url.QueryEscape(uidRaw)
	}
	xhdrSuffix := ""
	if xhdrRaw != "" {
		xhdrSuffix = "&xhdr=" + url.QueryEscape(xhdrRaw)
	}
	vpnSuffix := childVPNSuffix(q)

	var rewritten []string
	var lastExtInf float64
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// Track segment duration for progress estimation.
			if strings.HasPrefix(line, "#EXTINF:") {
				rest := strings.TrimPrefix(line, "#EXTINF:")
				if idx := strings.IndexAny(rest, ",\r\n"); idx >= 0 {
					rest = rest[:idx]
				}
				lastExtInf, _ = strconv.ParseFloat(rest, 64)
			}
			if strings.Contains(line, "URI=") {
				if m := reKeyURI.FindStringSubmatch(line); len(m) == 2 {
					absURI := proxyURLJoin(playlistBase, m[1])
					encURI := b64enc(absURI)
					var replacement string
					if strings.HasPrefix(line, "#EXT-X-KEY") {
						replacement = fmt.Sprintf("%s/key.key?data=%s&origin=%s&cookies=%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					} else if strings.HasPrefix(line, "#EXT-X-MAP") {
						// fMP4 init segment: binary resource, must go through segment proxy.
						// Routing it through playlist.m3u8 would return wrong Content-Type
						// and break playback silently (player never requests segments).
						replacement = fmt.Sprintf("%s/segment.ts?data=%s&origin=%s&cookies=%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					} else {
						replacement = fmt.Sprintf("%s/playlist.m3u8?data=%s&origin=%s&cookies=%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					}
					line = strings.Replace(line, m[1], core.AppendProxySig(replacement), 1)
				}
			}
			rewritten = append(rewritten, line)
		} else {
			// Segment URL — update session segment duration from preceding #EXTINF.
			if uidRaw != "" && lastExtInf > 0 {
				if s, ok := managers.Sessions.Get(uidRaw); ok {
					s.UpdateSegDuration(lastExtInf)
				}
			}
			lastExtInf = 0

			absURI := proxyURLJoin(playlistBase, line)
			encURI := b64enc(absURI)
			endpoint := "segment.ts"
			lower := strings.ToLower(absURI)
			if strings.Contains(lower, ".m3u8") || strings.Contains(lower, "/playlist/") {
				endpoint = "playlist.m3u8"
			}
			rewritten = append(rewritten, core.AppendProxySig(fmt.Sprintf("%s/%s?data=%s&origin=%s&cookies=%s%s%s%s",
				proxyBase, endpoint, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)))
		}
	}

	out := strings.Join(rewritten, "\n")
	if proxyDebug {
		log.Printf("[proxy/playlist] rewritten (%d lines):\n%s", len(rewritten), out)
	} else {
		log.Printf("[proxy/playlist] rewritten (%d lines)", len(rewritten))
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(w, out)
}

// ProxySegment godoc
//
//	@Summary		Proxy segmento TS
//	@Description	Scarica e streama un segmento video TS direttamente senza buffering. Senza autenticazione.
//	@Tags			Proxy HLS
//	@Param			data	query	string	true	"URL segmento upstream codificato in base64url"
//	@Param			origin	query	string	false	"Origin upstream codificato in base64url"
//	@Param			cookies	query	string	false	"Cookie upstream codificati in base64url"
//	@Produce		video/MP2T
//	@Success		200	{string}	string	"stream binario segmento"
//	@Failure		502	{string}	string	"upstream error"
//	@Router			/proxy/segment.ts [get]
func ProxySegment(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	log.Printf("[proxy/segment] INCOMING from=%s url=%s", r.RemoteAddr, logURL(r.URL.String()))
	if requireProxySig(w, r) {
		return
	}
	q := r.URL.Query()
	realURL := proxyB64Decode(q.Get("data"))
	realOrigin := proxyB64Decode(q.Get("origin"))
	uidRaw := q.Get("uid")
	useVPN := q.Get("vpn") == "1"
	egr := q.Get("egr")

	// Warm-cache hit: the pre-buffer step (prebuffer.go) already fetched and
	// normalized this exact segment during ResolveStream. Serve it from RAM —
	// this is what turns "3/8 segmenti" progress into a near-instant start.
	if body, ok := managers.Segments.Get(realURL); ok {
		ct := "video/MP2T"
		if lu := strings.ToLower(realURL); strings.Contains(lu, ".mp4") || strings.Contains(lu, ".m4s") || strings.Contains(lu, ".cmfv") || strings.Contains(lu, ".cmfa") {
			ct = "video/mp4"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		log.Printf("[proxy/segment] warm-cache hit (%d bytes) url=%s", len(body), logURL(realURL))
		if _, err := w.Write(body); err != nil && r.Context().Err() != nil {
			log.Printf("[proxy/segment] client disconnected on warm hit after %s url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
		}
		if uidRaw != "" {
			if s, ok := managers.Sessions.Get(uidRaw); ok {
				s.RecordFetch()
			}
		}
		return
	}

	segClient, clErr := upstreamSegmentClient(useVPN, egr)
	if clErr != nil {
		log.Printf("[proxy/segment] %v url=%s", clErr, logURL(realURL))
		http.Error(w, clErr.Error(), http.StatusBadGateway)
		return
	}

	// Cap concurrent upstream fetches per CDN host so a player's start-of-stream
	// burst can't trip a rate-limiting segment CDN. Released as soon as we're
	// done reading the upstream response.
	var segHost string
	if u, perr := url.Parse(realURL); perr == nil {
		segHost = u.Host
	}
	gateRelease, gerr := segAcquire(r.Context(), segHost)
	if gerr != nil {
		log.Printf("[proxy/segment] client gone while queued url=%s", logURL(realURL))
		return
	}
	defer gateRelease()

	// Retry only up to the point where we haven't written anything to the
	// player yet (no headers, no bytes sent): once io.Copy below starts, the
	// response is committed and can't be replayed. This catches transient
	// connection failures and bad statuses (DNS blip, edge handshake
	// failure, edge briefly down, a rate-limit 403 that clears after a
	// backoff) — not a mid-stream truncation after bytes have already
	// started flowing, which is a separate failure mode.
	const maxAttempts = 4
	var resp *http.Response
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// A fresh *http.Request per attempt: reusing one across multiple
		// Client.Do calls isn't part of the net/http contract.
		req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, q.Get("cookies"), q.Get("xhdr"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Annulla il download CDN se il client (player) chiude la connessione.
		req = req.WithContext(r.Context())

		var doErr error
		resp, doErr = segClient.Do(req)
		if doErr != nil {
			if r.Context().Err() != nil {
				// Client disconnesso prima che l'upstream rispondesse: se il player ha
				// mollato dopo pochi secondi, è quasi certamente perché l'host CDN di
				// questo segmento era lento/irraggiungibile — vale la pena tracciarlo,
				// a differenza di una disconnessione tardiva (utente ha chiuso il player).
				log.Printf("[proxy/segment] client disconnected after %s waiting on upstream (no response yet) url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
				return
			}
			if attempt < maxAttempts {
				log.Printf("[proxy/segment] fetch error (attempt %d/%d) url=%s: %v", attempt, maxAttempts, logURL(realURL), doErr)
				time.Sleep(retryBackoff(attempt))
				continue
			}
			log.Printf("[proxy/segment] fetch error url=%s: %v", logURL(realURL), doErr)
			http.Error(w, doErr.Error(), http.StatusBadGateway)
			return
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			if attempt < maxAttempts {
				d := segRetryBackoff(attempt, resp.StatusCode)
				log.Printf("[proxy/segment] upstream %d (attempt %d/%d, wait %s) url=%s", resp.StatusCode, attempt, maxAttempts, d, logURL(realURL))
				select {
				case <-time.After(d):
				case <-r.Context().Done():
					return
				}
				continue
			}
			log.Printf("[proxy/segment] upstream %d url=%s", resp.StatusCode, logURL(realURL))
			http.Error(w, fmt.Sprintf("upstream %d", resp.StatusCode), http.StatusBadGateway)
			return
		}
		break
	}
	defer resp.Body.Close()

	// Pass through the CDN's Content-Type so fMP4 segments (.mp4/.m4s) are
	// served as video/mp4, not video/MP2T. Players reject init segments and
	// media segments with the wrong MIME type.
	ct := resp.Header.Get("Content-Type")
	bodyModified := false
	switch {
	case ct == "":
		ct = "video/MP2T"
	case strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "application/"):
		// correct type — pass through as-is
	case strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "binary/"):
		// Some CDNs wrap MPEG-TS segments in a fake PNG container
		// (content-type: image/png, first byte 0x89). Browser players
		// like hls.js scan the full payload for TS sync bytes; strict demuxers like
		// ffmpeg/libmpv expect sync byte 0x47 at offset 0 and fail immediately.
		//
		// Read up to 64 KB, locate the first 0x47 followed by another 0x47 exactly
		// 188 bytes later (TS packet stride), then serve from that offset onwards.
		// If no TS pattern is found, pass the raw bytes anyway as a fallback.
		const scanLimit = 64 * 1024
		sniff, serr := io.ReadAll(io.LimitReader(resp.Body, scanLimit))
		tsOff := -1
		if serr == nil {
			tsOff = tsSyncOffset(sniff)
		}
		if tsOff > 0 {
			log.Printf("[proxy/segment] content-type=%q TS sync at offset %d — stripping header url=%s", ct, tsOff, logURL(realURL))
			sniff = sniff[tsOff:]
		} else {
			log.Printf("[proxy/segment] content-type=%q no TS sync found — passing raw bytes url=%s", ct, logURL(realURL))
		}
		ct = "video/MP2T"
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(sniff), resp.Body))
		bodyModified = true
	default:
		// text/html, text/javascript, application/octet-stream mislabelled,
		// etc. Some CDNs deliberately serve media segments under a non-media
		// content-type (or host them on a generic file host that labels by
		// extension). Sniff the payload for an actual media signature before
		// deciding: relay it as video if it really contains one, reject it as
		// a genuine error page only if it doesn't.
		const scanLimit = 64 * 1024
		sniff, serr := io.ReadAll(io.LimitReader(resp.Body, scanLimit))
		if serr != nil {
			log.Printf("[proxy/segment] content-type=%q read error: %v url=%s", ct, serr, logURL(realURL))
			http.Error(w, "segment read error", http.StatusBadGateway)
			return
		}
		off, mediaCT := mediaStartOffset(sniff)
		if off < 0 {
			preview := ""
			if os.Getenv("MYCELIUM_DEBUG") == "1" {
				n := len(sniff)
				if n > 400 {
					n = 400
				}
				preview = " body[:400]=" + strconv.Quote(string(sniff[:n]))
			}
			log.Printf("[proxy/segment] content-type=%q no media signature — rejecting url=%s%s", ct, logURL(realURL), preview)
			http.Error(w, "non-video segment", http.StatusBadGateway)
			return
		}
		if off > 0 {
			log.Printf("[proxy/segment] content-type=%q media at offset %d — stripping wrapper url=%s", ct, off, logURL(realURL))
			sniff = sniff[off:]
		} else {
			log.Printf("[proxy/segment] content-type=%q mislabelled media, relaying as %s url=%s", ct, mediaCT, logURL(realURL))
		}
		ct = mediaCT
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(sniff), resp.Body))
		bodyModified = true
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if !bodyModified {
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			w.Header().Set("Content-Length", cl)
		}
	}
	log.Printf("[proxy/segment] ok url=%s", logURL(realURL))
	if _, err := io.Copy(w, resp.Body); err != nil {
		if r.Context().Err() != nil {
			log.Printf("[proxy/segment] client disconnected mid-stream after %s url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
		} else {
			log.Printf("[proxy/segment] stream error: %v", err)
		}
	}

	// Aggiorna stima posizione nella sessione (best-effort, non bloccante).
	if uidRaw != "" {
		if s, ok := managers.Sessions.Get(uidRaw); ok {
			s.RecordFetch()
		}
	}
}

// ProxyKey godoc
//
//	@Summary		Proxy chiave HLS
//	@Description	Scarica e restituisce la chiave AES-128 per la decifratura dei segmenti HLS. Senza autenticazione.
//	@Tags			Proxy HLS
//	@Param			data	query	string	true	"URL chiave upstream codificato in base64url"
//	@Param			origin	query	string	false	"Origin upstream codificato in base64url"
//	@Param			cookies	query	string	false	"Cookie upstream codificati in base64url"
//	@Produce		application/octet-stream
//	@Success		200	{string}	string	"chiave binaria"
//	@Failure		502	{string}	string	"upstream error"
//	@Router			/proxy/key.key [get]
func ProxyKey(w http.ResponseWriter, r *http.Request) {
	if requireProxySig(w, r) {
		return
	}
	q := r.URL.Query()
	realURL := proxyB64Decode(q.Get("data"))
	realOrigin := proxyB64Decode(q.Get("origin"))
	useVPN := q.Get("vpn") == "1"
	egr := q.Get("egr")

	req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, q.Get("cookies"), q.Get("xhdr"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	keyClient, clErr := upstreamPlaylistClient(useVPN, egr)
	if clErr != nil {
		log.Printf("[proxy/key] %v url=%s", clErr, logURL(realURL))
		http.Error(w, clErr.Error(), http.StatusBadGateway)
		return
	}
	resp, err := keyClient.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			log.Printf("[proxy/key] fetch error url=%s: %v", logURL(realURL), err)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[proxy/key] upstream %d url=%s", resp.StatusCode, logURL(realURL))
		http.Error(w, fmt.Sprintf("upstream %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	// An HLS key is 16 bytes (AES-128) — cap generously anyway so a
	// misbehaving/malicious upstream streaming an unbounded body can't be used
	// to exhaust memory through this route.
	const keyReadLimit = 1 << 20 // 1 MiB
	data, err := io.ReadAll(io.LimitReader(resp.Body, keyReadLimit))
	if err != nil {
		log.Printf("[proxy/key] read error: %v", err)
		http.Error(w, "errore lettura chiave upstream", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(data)
}
