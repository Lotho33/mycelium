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

// NewPluginHTTPClient returns the *http.Client behind mycelium.network.*:
//   - uTLS with a Chrome ClientHello, ALPN forced to http/1.1;
//   - optional proxy (proxyURL "socks5://host:1080" or "http://host:8888",
//     "" = direct): with one, every connection goes through it and a proxy
//     failure fails the request, never falling back to direct;
//   - 30-second timeouts;
//   - on the direct path, the SSRF guard (core.GuardedDialContext): plugins
//     fetch arbitrary URLs, and this keeps them off loopback, link-local and
//     metadata addresses (LAN too with MYCELIUM_PROXY_BLOCK_PRIVATE=1). With
//     a proxy the proxy resolves the target, so no local check applies.
func NewPluginHTTPClient(proxyURL string) *http.Client {
	dial := core.ProxyDialer(proxyURL)
	if proxyURL == "" {
		dial = core.GuardedDialContext(dial)
	}

	dialTLSContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)

		rawConn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		// Chrome spec without h2 in ALPN: an http.Transport with DialTLSContext
		// can't speak HTTP/2.
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

// preservePOSTOnRedirect keeps the method and body across a 3xx: Go's
// default turns POST into GET on 301/302/303, which breaks API-style POSTs.
// It also reimplements the 10-redirect cap the default provides.
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
