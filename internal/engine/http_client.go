package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	tls "github.com/refraction-networking/utls"

	"mycelium/internal/core"
)

// NewScrapingClient returns an *http.Client tuned for broad site compatibility:
//   - uTLS with Chrome fingerprint (TLS ClientHello mimics Chrome), ALPN forced to http/1.1
//   - Optional VPN/proxy: pass a full URL ("socks5://warp:1080" or "http://host:8888"),
//     or "" to connect directly. When a proxy is set, EVERY connection is dialed through it
//     (both the plain and the TLS dial paths), so nothing can leak around the proxy — a proxy
//     failure fails the request rather than falling back to a direct connection.
//   - 30-second timeouts
func NewScrapingClient(proxyURL string) *http.Client {
	dial := core.ProxyDialer(proxyURL)

	dialTLSContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)

		rawConn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		// Clone Chrome spec, strip h2 from ALPN.
		// http.Transport with DialTLSContext cannot multiplex H2 — reading H2 SETTINGS
		// frames on the H1 code path produces "malformed HTTP response".
		spec, err := tls.UTLSIdToSpec(tls.HelloChrome_Auto)
		if err != nil {
			rawConn.Close()
			return nil, err
		}
		for i, ext := range spec.Extensions {
			if alpn, ok := ext.(*tls.ALPNExtension); ok {
				alpn.AlpnProtocols = []string{"http/1.1"}
				spec.Extensions[i] = alpn
				break
			}
		}

		uConn := tls.UClient(rawConn, &tls.Config{ServerName: host}, tls.HelloCustom)
		if err := uConn.ApplyPreset(&spec); err != nil {
			rawConn.Close()
			return nil, err
		}
		if err := uConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, err
		}
		return uConn, nil
	}

	transport := &http.Transport{
		DialContext:           dial, // plain HTTP requests also go through the proxy
		DialTLSContext:        dialTLSContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
	}

	return &http.Client{
		Transport:     transport,
		Timeout:       30 * time.Second,
		CheckRedirect: preservePOSTOnRedirect,
	}
}

// preservePOSTOnRedirect keeps the original method and body across a 3xx
// redirect. Go's default CheckRedirect downgrades POST (and PUT/PATCH) to GET
// on 301/302/303 — matching legacy browser form-submit behavior, but wrong
// for the JSON/AJAX-style POST calls plugins make (e.g. a POST to a
// search/listing endpoint): a site that 301s that specific route (protocol
// upgrade, path rename, WAF rule...) turns our POST into a GET, and the
// destination then rejects it with 405 — silently, since a 3xx followed by a
// 4xx just looks like "the request failed" without this being obvious from
// the final status alone. Providing any CheckRedirect replaces Go's built-in
// 10-redirect cap entirely, so it's reimplemented here too.
func preservePOSTOnRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}
	orig := via[0]
	switch orig.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		req.Method = orig.Method
		if orig.GetBody != nil {
			body, err := orig.GetBody()
			if err != nil {
				return err
			}
			req.Body = body
			req.ContentLength = orig.ContentLength
		}
		if ct := orig.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
	}
	return nil
}
