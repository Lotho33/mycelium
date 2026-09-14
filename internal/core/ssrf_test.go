package core

import (
	"context"
	"net"
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

// TestGuardedDialContextPinsResolvedIPPreventingRebinding simulates a
// classic DNS-rebinding attack: a name that answers a public IP on the first
// lookup (the one GuardedDialContext checks with BlockedProxyIP) and a
// blocked/private IP on a second, independent lookup — the shape of the
// TOCTOU this function used to have, back when it re-handed the hostname to
// base and let it resolve again on its own.
//
// The fake base dialer here plays the role of a plain *net.Dialer: if it
// were ever handed a hostname instead of a literal IP, it would resolve
// again itself (exactly like net.Dialer.DialContext does), and that second,
// uncontrolled lookup would hand back the rebinding address. The test
// asserts the resolver is called exactly once per dial (base never gets a
// chance to re-resolve) and that base is dialed on the literal IP from that
// first, verified lookup — proving the second lookup this attack depends on
// structurally cannot happen anymore.
func TestGuardedDialContextPinsResolvedIPPreventingRebinding(t *testing.T) {
	answers := []net.IP{
		net.ParseIP("93.184.216.34"), // 1st lookup: public — passes BlockedProxyIP
		net.ParseIP("127.0.0.1"),     // 2nd lookup (would-be rebind): loopback — blocked
	}
	calls := 0
	orig := lookupIPAddr
	defer func() { lookupIPAddr = orig }()
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
		"http://192.168.1.10/hls", // RFC1918, allowed by default (LAN Jellyfin/Plex)
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
