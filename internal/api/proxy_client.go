package api

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// Upstream HTTP clients of the HLS proxy — direct and VPN-routed, with
// separate pools for the small playlist/key requests and the segments.

var (
	upstreamMu   sync.Mutex
	proxySession *http.Client
	// segment client: longer timeout, persistent connections
	segmentClient *http.Client
)

const proxyUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// resetUpstreamClients drops the cached upstream clients so the next request
// rebuilds them (after a setting that changes their transport is saved).
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

// getProxySession returns the process-wide client for playlists and keys,
// stable for the whole process: its cookie jar keeps CDN session cookies.
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

// VPN-routed variants of the proxy clients, rebuilt when the proxy address
// changes.
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

// egressProxyAddr maps the &egr=<name> of a proxied URL to its proxy
// address. An empty name falls back to the single VPN proxy; a disabled or
// unknown egress is an error, so the fetch fails closed.
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

// upstreamPlaylistClient / upstreamSegmentClient pick the direct or VPN
// client, failing closed when the selected egress isn't usable.
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

// retryBackoff returns the delay before retry N+1 (150ms, then 300ms).
func retryBackoff(attempt int) time.Duration {
	return time.Duration(attempt) * 150 * time.Millisecond
}
