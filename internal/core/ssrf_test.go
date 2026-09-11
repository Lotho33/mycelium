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
