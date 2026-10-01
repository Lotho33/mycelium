package core

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"
)

// proxyBlockPrivate also blocks RFC1918/ULA targets (and CGNAT) on the image
// and direct-path proxies. Off by default: self-hosted sources on the LAN
// are legitimate.
var proxyBlockPrivate = os.Getenv("MYCELIUM_PROXY_BLOCK_PRIVATE") == "1"

// cgnatNet is 100.64.0.0/10, also used by Tailscale/Headscale peers
// (net.IP.IsPrivate doesn't cover it).
var cgnatNet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// lookupIPAddr is indirected so tests can fake DNS (e.g. rebinding).
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

// BlockedProxyIP reports whether ip must never be a fetch target: loopback,
// link-local (cloud metadata included), multicast and unspecified always;
// RFC1918/ULA/CGNAT only with MYCELIUM_PROXY_BLOCK_PRIVATE=1.
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
	// Explicit, although IsLinkLocalUnicast covers it.
	if ip.Equal(net.IPv4(169, 254, 169, 254)) {
		return true
	}
	if proxyBlockPrivate && (ip.IsPrivate() || cgnatNet.Contains(ip)) {
		return true
	}
	return false
}

// GuardedDialContext wraps base so that before every dial the host is
// resolved and every candidate IP is checked with BlockedProxyIP. The IP
// that passed the check is the one dialed, so DNS rebinding can't slip a
// second answer past it. TLS verification is unaffected: every caller
// verifies against the request's original host, not the dialed address.
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
		// Dial the verified IP, never the hostname again.
		verifiedIP := ips[0].IP.String()
		dialAddr := verifiedIP
		if splitErr == nil {
			dialAddr = net.JoinHostPort(verifiedIP, port)
		}
		return base(ctx, network, dialAddr)
	}
}

// checkURLNotSSRFTimeout bounds CheckURLNotSSRF's DNS lookup, so a slow
// resolver fails like any upstream hiccup instead of stalling the player.
const checkURLNotSSRFTimeout = 3 * time.Second

// CheckURLNotSSRF resolves rawURL's host and rejects it if any resolved IP
// is blocked by BlockedProxyIP. It is for the browser-service relay, which
// hands the URL to another process: that path gets the same check as a
// direct dial, whatever the service enforces itself. The relay resolves
// again when it fetches, so unlike GuardedDialContext this can't rule out
// DNS rebinding. A name that fails to resolve here is not treated as
// blocked: the relay's fetch surfaces the real failure.
func CheckURLNotSSRF(ctx context.Context, rawURL string) error {
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
	lookupCtx, cancel := context.WithTimeout(ctx, checkURLNotSSRFTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
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
