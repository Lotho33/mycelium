package core

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"
)

// proxyBlockPrivate additionally blocks RFC1918 / RFC4193 (ULA) targets on the
// image and standard-path stream proxies. Off by default: a self-hosted media
// server legitimately proxies posters and direct-play from a LAN Jellyfin/Plex.
// Turn it on for deployments whose sources are all public.
var proxyBlockPrivate = os.Getenv("MYCELIUM_PROXY_BLOCK_PRIVATE") == "1"

// lookupIPAddr is net.DefaultResolver.LookupIPAddr, indirected through a
// package variable purely so tests can substitute a fake resolver — e.g. to
// simulate a DNS-rebinding name that would answer differently across two
// independent lookups — without depending on real DNS or root-only /etc/hosts
// edits. Not configurable at runtime; production always uses the real
// resolver.
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

// BlockedProxyIP reports whether ip must never be a proxy fetch target. It
// always rejects loopback, link-local (covers the 169.254.169.254 cloud
// metadata endpoint), multicast and the unspecified address; RFC1918/ULA only
// when MYCELIUM_PROXY_BLOCK_PRIVATE=1.
func BlockedProxyIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() ||
		ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return true
	}
	// Explicit belt-and-suspenders for the classic SSRF target even though
	// IsLinkLocalUnicast already covers it.
	if ip.Equal(net.IPv4(169, 254, 169, 254)) {
		return true
	}
	if proxyBlockPrivate && ip.IsPrivate() {
		return true
	}
	return false
}

// GuardedDialContext wraps base so that, before every dial, the target host is
// resolved and every candidate IP is checked with BlockedProxyIP. A blocked
// address fails the dial with a clear error instead of connecting. This is
// defence-in-depth behind the /proxy HMAC — only URLs mycelium itself minted
// reach here — and the only SSRF guard on /img (whose URLs are minted client
// side and cannot be signed).
//
// The resolved IP that passed the check is what actually gets dialed: once a
// hostname resolves, the returned dialer calls base with that literal IP
// (same port as the original addr) instead of handing the hostname back to
// it. This closes a DNS-rebinding TOCTOU that used to exist here — base was
// previously called with the original hostname again, so a plain
// *net.Dialer (or any dialer that itself resolves) re-resolved the name a
// second time, independently of the lookup just checked above. A malicious
// name with a very low TTL (or a resolver that alternates answers) could
// return a public IP for the first lookup — passing BlockedProxyIP — and a
// blocked address (loopback, link-local/cloud-metadata, or, in strict mode,
// RFC1918) for the second, uncontrolled one that base actually connected to.
// Dialing the literal IP here means there is no second, unverified
// resolution left to race.
//
// This is safe for TLS at every current call site because none of them let
// base itself pick the certificate hostname from the dialed address: a plain
// *http.Transport with no DialTLSContext (internal/api/image_proxy.go's
// imgClient, and the standard branch of internal/api/upstream_transport.go's
// newUTLSTransport) tracks the request's original host separately in its
// connectMethod and uses that — not the net.Conn's actual remote IP — for
// SNI and certificate verification; internal/engine/http_client.go's
// NewScrapingClient dials through this function and then does its own uTLS
// handshake over the raw conn with an explicit ServerName taken from the
// original addr, never from the conn. Pinning the dial to an IP therefore
// never touches hostname verification at any of the three call sites. A
// caller that ever let TLS infer the hostname from the dialed address itself
// (e.g. a bare tls.DialWithDialer-style helper with no ServerName override)
// would need its own fix — none of the current callers do this.
func GuardedDialContext(base func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		base = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			host = addr
		}
		if ip := net.ParseIP(host); ip != nil {
			if BlockedProxyIP(ip) {
				return nil, fmt.Errorf("proxy: refusing to dial blocked address %s", ip)
			}
			return base(ctx, network, addr)
		}
		ips, err := lookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("proxy: %s did not resolve to any address", host)
		}
		for _, a := range ips {
			if BlockedProxyIP(a.IP) {
				return nil, fmt.Errorf("proxy: %s resolves to blocked address %s", host, a.IP)
			}
		}
		// All candidates passed; dial the first one directly instead of
		// re-handing the hostname to base (see doc comment above).
		verifiedIP := ips[0].IP.String()
		dialAddr := verifiedIP
		if splitErr == nil {
			dialAddr = net.JoinHostPort(verifiedIP, port)
		}
		return base(ctx, network, dialAddr)
	}
}

// CheckURLNotSSRF resolves rawURL's host and rejects it with an error if any
// resolved IP is blocked by BlockedProxyIP. Unlike GuardedDialContext, it does
// not dial anything — it's for the cobweb relay path, which doesn't dial the
// upstream itself but hands the URL off to the cobweb sidecar to fetch on
// mycelium's behalf. Calling this first, before that hand-off, gives that path
// the same Go-side SSRF check the direct dial path already has, independent of
// whatever cobweb does or doesn't enforce itself.
//
// Same TOCTOU caveat as GuardedDialContext: the name is resolved once here and
// again by cobweb when it actually fetches, so a DNS-rebinding attacker can
// still slip a blocked address past this check. That's a separate, already
// tracked gap — this function only closes the "no check at all" hole.
//
// A hostname that fails to resolve here is not treated as blocked: there is
// nothing to check it against, and cobweb's own fetch (which resolves again,
// possibly through a different path — its own DNS, a proxy) will surface the
// real failure if the name truly doesn't resolve.
func CheckURLNotSSRF(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("proxy: invalid URL %q: %w", rawURL, err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("proxy: URL %q has no host", rawURL)
	}
	if ip := net.ParseIP(host); ip != nil {
		if BlockedProxyIP(ip) {
			return fmt.Errorf("proxy: refusing to fetch blocked address %s", ip)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return nil
	}
	for _, a := range ips {
		if BlockedProxyIP(a.IP) {
			return fmt.Errorf("proxy: %s resolves to blocked address %s", host, a.IP)
		}
	}
	return nil
}
