package core

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestBlockedProxyIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"169.254.169.254", true}, // cloud metadata
		{"169.254.10.20", true},   // link-local
		{"fe80::1", true},
		{"224.0.0.1", true}, // multicast
		{"ff02::1", true},
		{"8.8.8.8", false},
		{"93.184.216.34", false}, // example.com
		{"1.1.1.1", false},
		{"192.168.1.10", false}, // RFC1918 allowed by default
		{"10.0.0.5", false},
		{"172.16.0.9", false},
		{"fd00::1", false}, // ULA allowed by default
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := BlockedProxyIP(ip); got != c.blocked {
			t.Errorf("BlockedProxyIP(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
	if !BlockedProxyIP(nil) {
		t.Error("BlockedProxyIP(nil) should be true (fail closed)")
	}
}

func TestGuardedDialContextBlocksLiteralInternal(t *testing.T) {
	called := false
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		called = true
		return nil, nil
	}
	dial := GuardedDialContext(base)

	for _, addr := range []string{"127.0.0.1:80", "169.254.169.254:80", "[::1]:443"} {
		if _, err := dial(context.Background(), "tcp", addr); err == nil {
			t.Errorf("dial(%s) succeeded, want blocked", addr)
		}
	}
	if called {
		t.Error("base dialer was called for a blocked address")
	}
}

func TestGuardedDialContextAllowsPublicLiteral(t *testing.T) {
	called := false
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		called = true
		return nil, nil
	}
	dial := GuardedDialContext(base)
	if _, err := dial(context.Background(), "tcp", "93.184.216.34:80"); err != nil {
		t.Fatalf("dial of a public literal errored: %v", err)
	}
	if !called {
		t.Error("base dialer was not called for an allowed address")
	}
}

// DNS rebinding: a name answering a public IP on the first lookup and a
// blocked one on a second. GuardedDialContext resolves once and dials the
// verified literal IP, so the second lookup never happens.
func TestGuardedDialContextPinsResolvedIPPreventingRebinding(t *testing.T) {
	answers := []net.IP{
		net.ParseIP("93.184.216.34"), // 1st lookup: public — passes BlockedProxyIP
		net.ParseIP("127.0.0.1"),     // 2nd lookup (would-be rebind): loopback — blocked
	}
	calls := 0
	orig := lookupIPAddr
	defer func() { lookupIPAddr = orig }()
	guardDNSCache.Delete("rebind.example")
	t.Cleanup(func() { guardDNSCache.Delete("rebind.example") })
	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		idx := calls
		if idx >= len(answers) {
			idx = len(answers) - 1
		}
		calls++
		return []net.IPAddr{{IP: answers[idx]}}, nil
	}

	var dialedAddr string
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Mimic what a plain *net.Dialer (or any dialer that resolves on its
		// own) would do if handed a hostname here: resolve it again,
		// independently of the check above.
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if net.ParseIP(host) == nil {
			if _, err := lookupIPAddr(ctx, host); err != nil {
				return nil, err
			}
		}
		dialedAddr = addr
		return nil, nil
	}

	dial := GuardedDialContext(base)
	if _, err := dial(context.Background(), "tcp", "rebind.example:80"); err != nil {
		t.Fatalf("dial errored: %v", err)
	}

	if calls != 1 {
		t.Fatalf("resolver called %d times, want exactly 1 — base must never get a chance to re-resolve", calls)
	}
	host, _, err := net.SplitHostPort(dialedAddr)
	if err != nil {
		t.Fatalf("dialed addr %q has no port: %v", dialedAddr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("base was dialed with a non-literal host %q — the TOCTOU is reopened", host)
	}
	if ip.String() != "93.184.216.34" {
		t.Fatalf("base dialed %s, want the first-lookup (verified) IP 93.184.216.34", ip)
	}
}

func TestCheckURLNotSSRFBlocksInternal(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1/secret",
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/",
	} {
		if err := CheckURLNotSSRF(context.Background(), raw); err == nil {
			t.Errorf("CheckURLNotSSRF(%s) = nil, want error", raw)
		}
	}
}

func TestCheckURLNotSSRFAllowsPublicAndPrivateByDefault(t *testing.T) {
	for _, raw := range []string{
		"https://93.184.216.34/",  // example.com's IP, literal
		"http://192.168.1.10/hls", // RFC1918, allowed by default (LAN media server)
	} {
		if err := CheckURLNotSSRF(context.Background(), raw); err != nil {
			t.Errorf("CheckURLNotSSRF(%s) errored: %v", raw, err)
		}
	}
}

func TestCheckURLNotSSRFInvalidURL(t *testing.T) {
	if err := CheckURLNotSSRF(context.Background(), "http://[::1"); err == nil {
		t.Error("CheckURLNotSSRF with a malformed URL should error")
	}
	if err := CheckURLNotSSRF(context.Background(), "not-a-url-at-all"); err == nil {
		// A bare string with no scheme parses to a URL with an empty host —
		// must be rejected too (empty host is not something safe to hand off).
		t.Error("CheckURLNotSSRF with no host should error")
	}
}

func TestBlockedProxyIP_BlockPrivateCoversCGNAT(t *testing.T) {
	orig := proxyBlockPrivate
	t.Cleanup(func() { proxyBlockPrivate = orig })

	proxyBlockPrivate = true
	for _, s := range []string{"100.64.0.1", "100.101.102.103", "100.127.255.254", "10.0.0.1", "192.168.1.1"} {
		if !BlockedProxyIP(net.ParseIP(s)) {
			t.Errorf("%s must be blocked with MYCELIUM_PROXY_BLOCK_PRIVATE=1", s)
		}
	}
	if BlockedProxyIP(net.ParseIP("100.128.0.1")) {
		t.Error("100.128.0.1 is outside 100.64/10 and must not be blocked")
	}

	proxyBlockPrivate = false
	if BlockedProxyIP(net.ParseIP("100.64.0.1")) {
		t.Error("CGNAT must stay allowed by default, like RFC1918")
	}
}

// fakeDNS swaps both guard resolvers for the test and clears the cache.
func fakeDNS(t *testing.T, public, local func(context.Context, string) ([]net.IPAddr, error)) {
	t.Helper()
	origPub, origLocal := lookupIPAddr, lookupIPAddrLocal
	lookupIPAddr, lookupIPAddrLocal = public, local
	clear := func() { guardDNSCache.Range(func(k, _ any) bool { guardDNSCache.Delete(k); return true }) }
	clear()
	t.Cleanup(func() {
		lookupIPAddr, lookupIPAddrLocal = origPub, origLocal
		clear()
	})
}

func TestGuardLookupFallsBackToLocalResolverAndCaches(t *testing.T) {
	pubCalls, localCalls := 0, 0
	fakeDNS(t,
		func(context.Context, string) ([]net.IPAddr, error) {
			pubCalls++
			return nil, &net.DNSError{Err: "no such host", Name: "jellyfin", IsNotFound: true}
		},
		func(context.Context, string) ([]net.IPAddr, error) {
			localCalls++
			return []net.IPAddr{{IP: net.ParseIP("172.18.0.5")}}, nil
		},
	)
	for i := 0; i < 3; i++ {
		ips, err := guardLookup(context.Background(), "jellyfin")
		if err != nil || len(ips) != 1 || ips[0].IP.String() != "172.18.0.5" {
			t.Fatalf("guardLookup = %v, %v; want the local resolver's answer", ips, err)
		}
	}
	if pubCalls != 1 || localCalls != 1 {
		t.Fatalf("resolver calls public=%d local=%d, want 1/1 (later lookups from cache)", pubCalls, localCalls)
	}
}

func TestGuardLookupDoesNotCacheFailures(t *testing.T) {
	calls := 0
	fail := func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return nil, &net.DNSError{Err: "timeout", Name: "x", IsTimeout: true}
	}
	fakeDNS(t, fail, fail)
	for i := 0; i < 2; i++ {
		if _, err := guardLookup(context.Background(), "cdn.example"); err == nil {
			t.Fatal("want an error")
		}
	}
	if calls != 4 {
		t.Fatalf("resolver calls = %d, want 4 (no negative caching)", calls)
	}
}

func TestGuardedDialContextFallsThroughToOtherFamily(t *testing.T) {
	fakeDNS(t,
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("2001:4860::1")}, {IP: net.ParseIP("93.184.216.34")}}, nil
		},
		func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("unused") },
	)
	orig := egressPreferIPv6()
	SetEgressPreferIPv6(true)
	t.Cleanup(func() { SetEgressPreferIPv6(orig) })

	var dialed []string
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		if strings.HasPrefix(addr, "[") {
			return nil, errors.New("network unreachable")
		}
		return nil, nil
	}
	if _, err := GuardedDialContext(base)(context.Background(), "tcp", "cdn.example:443"); err != nil {
		t.Fatalf("dial errored: %v", err)
	}
	want := []string{"[2001:4860::1]:443", "93.184.216.34:443"}
	if strings.Join(dialed, ",") != strings.Join(want, ",") {
		t.Fatalf("dialed %v, want %v", dialed, want)
	}
}
