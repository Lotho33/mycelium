package api

import (
	"bytes"
	"encoding/json"
	nethttp "net/http"
	"os"
	"strings"
	"time"

	"mycelium/internal/core"
)

// Upstream fetches (HLS playlists, segments, keys) are relayed by cobweb's
// POST /v1/fetch so the TLS/HTTP2 impersonation happens once — in cobweb, with
// the same wreq/BoringSSL fingerprint it used to resolve the stream. mycelium
// keeps all the caching / dedup / retry / content sniffing; only the wire hop
// moves.
//
// The one escape hatch is MYCELIUM_HTTP_PROFILE=standard (env only, no UI): a
// plain net/http path for a deployment without a cobweb sidecar, or for
// debugging. Any other value — including the default — means "via cobweb".
func httpProfileStandard() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("MYCELIUM_HTTP_PROFILE")), "standard")
}

// compatUA is the User-Agent used on the standard path (the cobweb path lets
// cobweb's own profile pick a UA that matches its fingerprint). buildProxyRequest
// forces it after overlaying sniffed headers, except when a session cookie
// (cf_clearance) is present — that clearance is bound to the UA that solved it.
const compatUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// utlsTransportOpts carries the connection-pool / timeout knobs for the
// standard net/http transport. The cobweb path uses its own shared client.
type utlsTransportOpts struct {
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	IdleConnTimeout       time.Duration
	ResponseHeaderTimeout time.Duration
}

// stdTransport is the net/http transport for the standard (no-cobweb) path.
func stdTransport(opts utlsTransportOpts) *nethttp.Transport {
	return &nethttp.Transport{
		MaxIdleConns:          opts.MaxIdleConns,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		IdleConnTimeout:       opts.IdleConnTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		ForceAttemptHTTP2:     true,
	}
}

// newUTLSTransport builds the transport for direct (non-proxied) upstream
// fetches. cobweb sidecar by default; plain net/http (DNS via core.CFDialContext)
// only when MYCELIUM_HTTP_PROFILE=standard.
func newUTLSTransport(opts utlsTransportOpts) nethttp.RoundTripper {
	if httpProfileStandard() {
		t := stdTransport(opts)
		// SSRF guard on the direct path only: the proxy path (below) and the
		// cobweb path resolve the target themselves, so the check lives there.
		t.DialContext = core.GuardedDialContext(core.CFDialContext)
		return t
	}
	return &cobwebRoundTripper{}
}

// newUTLSProxyTransport builds a transport whose every fetch is routed through
// proxyURL (e.g. socks5://proxy:1080). On the standard path core.ProxyDialer
// never falls back to a direct connection, so a request fails rather than
// leaking around a down proxy; on the cobweb path the proxy is handed to cobweb.
func newUTLSProxyTransport(proxyURL string, opts utlsTransportOpts) nethttp.RoundTripper {
	if httpProfileStandard() {
		t := stdTransport(opts)
		t.DialContext = core.ProxyDialer(proxyURL)
		return t
	}
	return &cobwebRoundTripper{proxyURL: proxyURL}
}

// ─── cobweb relay ───────────────────────────────────────────────────────────

// cobwebBase is the cobweb sidecar's base URL — same source as cmd/server/main.go.
func cobwebBase() string {
	if a := strings.TrimSpace(os.Getenv("COBWEB_ADDR")); a != "" {
		return strings.TrimRight(a, "/")
	}
	return "http://localhost:8191"
}

// cobwebFetchClient talks to the local cobweb sidecar — a plain internal call,
// no impersonation. No client Timeout: the wrapping proxy http.Client bounds
// the whole round-trip.
var cobwebFetchClient = &nethttp.Client{
	Transport: &nethttp.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

// cobwebRoundTripper turns an outbound upstream GET into a POST to cobweb's
// /v1/fetch. All caching / dedup / retry stays in proxy.go; this only swaps how
// the bytes come off the wire.
type cobwebRoundTripper struct {
	proxyURL string // "" = direct
}

// hop-by-hop + headers cobweb/wreq manage themselves.
var cobwebDropHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-connection": true,
	"transfer-encoding": true, "upgrade": true, "host": true,
	"accept-encoding": true,
}

func (t *cobwebRoundTripper) RoundTrip(req *nethttp.Request) (*nethttp.Response, error) {
	// Keep the request's User-Agent only when a session cookie (cf_clearance)
	// is riding along — it's bound to that UA. Otherwise drop it (and its
	// client-hints) so cobweb's wreq profile supplies a set consistent with
	// its own TLS fingerprint.
	keepUA := strings.Contains(req.Header.Get("Cookie"), "cf_clearance")

	hdr := make(map[string]string, len(req.Header))
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if len(vs) == 0 || cobwebDropHeaders[lk] {
			continue
		}
		if !keepUA && (lk == "user-agent" || strings.HasPrefix(lk, "sec-ch-ua")) {
			continue
		}
		hdr[k] = vs[0]
	}

	timeoutMs := int64(120000)
	if dl, ok := req.Context().Deadline(); ok {
		if ms := time.Until(dl).Milliseconds(); ms > 0 {
			timeoutMs = ms
		}
	}

	payload := map[string]any{
		"url":        req.URL.String(),
		"headers":    hdr,
		"timeout_ms": timeoutMs,
		// mycelium already threads the upstream Cookie via buildProxyRequest.
		"use_jar": false,
	}
	if t.proxyURL != "" {
		payload["proxy_url"] = t.proxyURL
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	fReq, err := nethttp.NewRequestWithContext(req.Context(), nethttp.MethodPost, cobwebBase()+"/v1/fetch", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	fReq.Header.Set("Content-Type", "application/json")

	resp, err := cobwebFetchClient.Do(fReq)
	if err != nil {
		return nil, err
	}
	// cobweb already mirrored the upstream status, streamed the body, and
	// stripped content-encoding/length — hand it straight back.
	resp.Request = req
	return resp, nil
}
