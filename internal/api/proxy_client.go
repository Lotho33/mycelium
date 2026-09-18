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

// Upstream HTTP clients the HLS proxy fetches through — direct and VPN-routed,
// separate pools for the small playlist/key requests vs. the larger/longer
// segment ones. Split out of proxy.go (P2-2): this is lifecycle/connection
// management, independent of the three /proxy/* handlers that use it.

var (
	upstreamMu   sync.Mutex
	proxySession *http.Client
	// Client dedicato ai segmenti TS: timeout più alto, connessioni persistenti
	segmentClient *http.Client
)

const proxyUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

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
