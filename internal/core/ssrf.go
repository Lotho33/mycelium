package core

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"
	"time"
)

// proxyBlockPrivate also blocks RFC1918/ULA targets (and CGNAT) on the image
// and direct-path proxies. Off by default: self-hosted sources on the LAN
// are legitimate.
var proxyBlockPrivate = os.Getenv("MYCELIUM_PROXY_BLOCK_PRIVATE") == "1"

// cgnatNet is 100.64.0.0/10, also used by Tailscale/Headscale peers
// (net.IP.IsPrivate doesn't cover it).
var cgnatNet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// lookupIPAddr is the public-resolver lookup, indirected so tests can fake
// DNS (e.g. rebinding). It reads net.DefaultResolver at call time: a method
// value taken at package init would bind the OS resolver, since cmd/server's
// init() swaps in the public one only after this package is initialised.
var lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// lookupIPAddrLocal is the OS resolver (/etc/resolv.conf: the container
// engine's DNS in Docker, the LAN resolver otherwise). Only a fallback, for
// container names and LAN/split-horizon records public resolvers can't see.
var lookupIPAddrLocal = (&net.Resolver{PreferGo: true}).LookupIPAddr

// guardDNSCache caches the guard's lookups ("host" -> guardDNSEntry) for
// dnsCacheTTL. Without it every new upstream connection, and every segment
// handed to the browser relay, cost a fresh query; a rate-limited LAN
// resolver dropping one stalled the request for the resolver retransmit.
// Caching costs nothing against rebinding: GuardedDialContext dials exactly
// the addresses it checked.
var guardDNSCache sync.Map

type guardDNSEntry struct {
	ips     []net.IPAddr
	expires time.Time
}

// guardLookup resolves host through the public resolvers, falling back to
// the OS resolver, with a short cache. Failures are not cached.
func guardLookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	if v, ok := guardDNSCache.Load(host); ok {
		e := v.(guardDNSEntry)
		if time.Now().Before(e.expires) {
			return e.ips, nil
		}
		guardDNSCache.Delete(host)
	}
	ips, err := lookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if local, lerr := lookupIPAddrLocal(ctx, host); lerr == nil && len(local) > 0 {
			ips, err = local, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if len(ips) > 0 {
		guardDNSCache.Store(host, guardDNSEntry{ips: ips, expires: time.Now().Add(dnsCacheTTL)})
	}
	return ips, nil
}

// dialCandidates orders the verified addresses for dialing: the first one of
// the preferred family, then the first one of the other family, so a
// black-holed family falls through like CFDialContext does.
func dialCandidates(ips []net.IPAddr, preferV6 bool) []net.IP {
	var first, other net.IP
	for _, a := range ips {
		if (a.IP.To4() == nil) == preferV6 {
			if first == nil {
				first = a.IP
			}
		} else if other == nil {
			other = a.IP
		}
	}
	out := make([]net.IP, 0, 2)
	for _, ip := range []net.IP{first, other} {
		if ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

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
		ips, err := guardLookup(ctx, host)
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
		// Dial the verified IPs, never the hostname again. All but the last
		// attempt are bounded, so a dead address falls through quickly.
		cands := dialCandidates(ips, egressPreferIPv6())
		var lastErr error
		for i, ip := range cands {
			dialAddr := ip.String()
			if splitErr == nil {
				dialAddr = net.JoinHostPort(dialAddr, port)
			}
			attemptCtx, cancel := ctx, context.CancelFunc(func() {})
			if i < len(cands)-1 {
				attemptCtx, cancel = context.WithTimeout(ctx, 6*time.Second)
			}
			conn, derr := base(attemptCtx, network, dialAddr)
			cancel()
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
			if ctx.Err() != nil {
				break
			}
		}
		return nil, lastErr
	}
}

// checkURLNotSSRFTimeout bounds CheckURLNotSSRF's DNS lookup (a cache miss
// only, see guardLookup), so a slow resolver doesn't stall the player.
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
	ips, err := guardLookup(lookupCtx, host)
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
