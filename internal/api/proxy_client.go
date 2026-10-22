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

// proxyUserAgent is the User-Agent of the image proxy's fetches: a common
// desktop browser string, since many image hosts reject unknown clients.
const proxyUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// resetUpstreamClients drops the cached upstream clients so the next request
// rebuilds them (after a setting that changes their transport is saved).
func resetUpstreamClients() {
	upstreamMu.Lock()
	proxySession = nil
	segmentClient = nil
	upstreamMu.Unlock()

	vpnMu.Lock()
	old := vpnPairs
	vpnPairs = map[string]vpnPair{}
	vpnMu.Unlock()
	for _, p := range old {
		p.close()
	}
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
			Transport: newUpstreamTransport(upstreamTransportOpts{
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
		// No total Timeout: it would cut a streamed body that is still flowing
		// (a long progressive file). Each attempt bounds its wait for headers,
		// buffering is bounded by the retry budget, and a streamed body by
		// segmentBodyIdle (proxy_segment_stream.go).
		segmentClient = &http.Client{
			Jar: jar,
			Transport: newUpstreamTransport(upstreamTransportOpts{
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				IdleConnTimeout:       120 * time.Second,
				ResponseHeaderTimeout: 15 * time.Second,
			}),
		}
	}
	return segmentClient
}

// VPN-routed variants of the proxy clients, one pair per egress proxy (two
// egresses can serve streams at the same time; each pair keeps its
// connections and cookies).
type vpnPair struct{ session, segment *http.Client }

func (p vpnPair) close() {
	p.session.CloseIdleConnections()
	p.segment.CloseIdleConnections()
}

var (
	vpnPairs = map[string]vpnPair{}
	vpnMu    sync.Mutex
)

// vpnClients returns the (playlist/key, segment) clients routed through proxyURL.
func vpnClients(proxyURL string) (*http.Client, *http.Client) {
	vpnMu.Lock()
	defer vpnMu.Unlock()
	if p, ok := vpnPairs[proxyURL]; ok {
		return p.session, p.segment
	}
	jar1, _ := cookiejar.New(nil)
	session := &http.Client{Timeout: 15 * time.Second, Jar: jar1, Transport: newUpstreamProxyTransport(proxyURL, upstreamTransportOpts{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
	})}

	jar2, _ := cookiejar.New(nil)
	// No total Timeout: see getSegmentClient.
	segment := &http.Client{Jar: jar2, Transport: newUpstreamProxyTransport(proxyURL, upstreamTransportOpts{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       120 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	})}
	// Proxy addresses come from the egress registry: a handful at most.
	vpnPairs[proxyURL] = vpnPair{session, segment}
	return session, segment
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
