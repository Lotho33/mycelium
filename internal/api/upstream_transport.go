package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	nethttp "net/http"
	"os"
	"strings"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// Upstream fetches (HLS playlists, segments, keys) go out through a plain
// net/http transport — or, when an optional browser service is configured
// (MYCELIUM_BROWSER_URL, see internal/managers/browser_client.go), through
// its POST /v1/fetch relay, so they leave from the same HTTP client the
// service used to resolve the stream. mycelium keeps all the caching /
// dedup / retry / content sniffing either way; only the wire hop moves.
//
// MYCELIUM_HTTP_PROFILE=standard (env only, no UI) forces the direct path
// even with a browser service configured — for debugging.
func httpProfileStandard() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("MYCELIUM_HTTP_PROFILE")), "standard")
}

// relayEnabled reports whether upstream fetches go through the browser
// service's /v1/fetch relay.
func relayEnabled() bool {
	return managers.BrowserClient != nil && !httpProfileStandard()
}

// compatUA is the User-Agent of the direct path (the relay picks its own).
// buildProxyRequest forces it unless a session cookie (cf_clearance) or the
// X-Force-User-Agent sentinel pins the captured one.
const compatUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

// utlsTransportOpts carries the connection-pool / timeout knobs for the
// direct net/http transport. The relay path uses its own shared client.
type utlsTransportOpts struct {
	MaxIdleConns          int
	MaxIdleConnsPerHost   int
	IdleConnTimeout       time.Duration
	ResponseHeaderTimeout time.Duration
}

// stdTransport is the net/http transport for the direct path.
func stdTransport(opts utlsTransportOpts) *nethttp.Transport {
	return &nethttp.Transport{
		MaxIdleConns:          opts.MaxIdleConns,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		IdleConnTimeout:       opts.IdleConnTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		ForceAttemptHTTP2:     true,
	}
}

// stripPinUA removes the internal pinUAHeader marker before a request goes
// out on the plain net/http path (which keeps the UA as-is anyway).
type stripPinUA struct{ rt nethttp.RoundTripper }

func (s stripPinUA) RoundTrip(req *nethttp.Request) (*nethttp.Response, error) {
	if req.Header.Get(pinUAHeader) != "" {
		req = req.Clone(req.Context())
		req.Header.Del(pinUAHeader)
	}
	return s.rt.RoundTrip(req)
}

// newUTLSTransport builds the transport for direct (non-proxied) upstream
// fetches: plain net/http (DNS via core.CFDialContext, SSRF-guarded), or the
// browser service's relay when one is configured.
func newUTLSTransport(opts utlsTransportOpts) nethttp.RoundTripper {
	if !relayEnabled() {
		t := stdTransport(opts)
		// SSRF guard on the direct path (the relay checks in RoundTrip; a proxy
		// resolves the target itself).
		t.DialContext = core.GuardedDialContext(core.CFDialContext)
		return stripPinUA{t}
	}
	return &relayRoundTripper{}
}

// newUTLSProxyTransport builds a transport whose every fetch is routed through
// proxyURL (e.g. socks5://proxy:1080). On the direct path core.ProxyDialer
// never falls back to a direct connection, so a request fails rather than
// leaking around a down proxy; on the relay path the proxy is handed to the
// browser service.
func newUTLSProxyTransport(proxyURL string, opts utlsTransportOpts) nethttp.RoundTripper {
	if !relayEnabled() {
		t := stdTransport(opts)
		t.DialContext = core.ProxyDialer(proxyURL)
		return stripPinUA{t}
	}
	return &relayRoundTripper{proxyURL: proxyURL}
}

// ─── browser-service relay ──────────────────────────────────────────────────

// relayFetchClient talks to the browser service's /v1/fetch — a plain
// internal call. No client Timeout: the wrapping proxy http.Client bounds the
// whole round-trip.
var relayFetchClient = &nethttp.Client{
	Transport: &nethttp.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

// relayRoundTripper turns an outbound upstream GET into a POST to the browser
// service's /v1/fetch. All caching / dedup / retry stays in proxy.go; this
// only swaps how the bytes come off the wire.
type relayRoundTripper struct {
	proxyURL string // "" = direct
}

// hop-by-hop + headers the relay's own HTTP client manages.
var relayDropHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-connection": true,
	"transfer-encoding": true, "upgrade": true, "host": true,
	"accept-encoding": true,
}

func (t *relayRoundTripper) RoundTrip(req *nethttp.Request) (*nethttp.Response, error) {
	// Defence in depth: refuse to hand the browser service a target that
	// resolves to a blocked address, whatever its own guard does.
	if err := core.CheckURLNotSSRF(req.Context(), req.URL.String()); err != nil {
		return nil, fmt.Errorf("browser relay: %w", err)
	}

	// Keep the request's User-Agent only when a session cookie (cf_clearance)
	// is riding along — it's bound to that UA. Otherwise drop it (and its
	// client-hints) so the relay's HTTP client supplies a set consistent
	// with its own.
	keepUA := strings.Contains(req.Header.Get("Cookie"), "cf_clearance") || req.Header.Get(pinUAHeader) != ""

	hdr := make(map[string]string, len(req.Header))
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if len(vs) == 0 || relayDropHeaders[lk] || lk == strings.ToLower(pinUAHeader) {
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

	fReq, err := nethttp.NewRequestWithContext(req.Context(), nethttp.MethodPost, managers.BrowserClient.BaseURL()+"/v1/fetch", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	fReq.Header.Set("Content-Type", "application/json")
	managers.BrowserClient.ApplyAuth(fReq)

	resp, err := relayFetchClient.Do(fReq)
	if err != nil {
		return nil, err
	}
	// The relay already mirrored the upstream status, streamed the body, and
	// stripped content-encoding/length — hand it straight back.
	resp.Request = req
	return resp, nil
}
