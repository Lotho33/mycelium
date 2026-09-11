package core

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

// proxyBlockPrivate additionally blocks RFC1918 / RFC4193 (ULA) targets on the
// image and standard-path stream proxies. Off by default: a self-hosted media
// server legitimately proxies posters and direct-play from a LAN Jellyfin/Plex.
// Turn it on for deployments whose sources are all public.
var proxyBlockPrivate = os.Getenv("MYCELIUM_PROXY_BLOCK_PRIVATE") == "1"

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
// It intentionally does NOT pin the resolved IP into the dial (no TOCTOU
// hardening): the realistic threat is a literal internal URL, which is fully
// blocked; DNS-rebinding a name past this check is out of scope here.
func GuardedDialContext(base func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		base = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if ip := net.ParseIP(host); ip != nil {
			if BlockedProxyIP(ip) {
				return nil, fmt.Errorf("proxy: refusing to dial blocked address %s", ip)
			}
			return base(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, a := range ips {
			if BlockedProxyIP(a.IP) {
				return nil, fmt.Errorf("proxy: %s resolves to blocked address %s", host, a.IP)
			}
		}
		return base(ctx, network, addr)
	}
}
