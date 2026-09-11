package pileus

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

func TestIPRateLimiter_BurstThenBlock(t *testing.T) {
	l := newIPRateLimiter(30, 5) // 5 burst, refills 0.5/s

	for i := 0; i < 5; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed within burst", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("6th request should be blocked — burst exhausted")
	}
	// A different IP has its own bucket.
	if !l.allow("5.6.7.8") {
		t.Fatal("a fresh IP must not be affected by another IP's bucket")
	}
}

func TestPeerIP_TrustsXFFOnlyFromLoopback(t *testing.T) {
	mkCtx := func(addr string, xff string) context.Context {
		ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: fakeAddr(addr)})
		if xff != "" {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("x-forwarded-for", xff))
		}
		return ctx
	}

	// Loopback peer (the gRPC-web bridge) → trust the forwarded client IP.
	if got := peerIP(mkCtx("127.0.0.1:5555", "9.9.9.9, 10.0.0.1")); got != "9.9.9.9" {
		t.Errorf("loopback peer: peerIP = %q, want 9.9.9.9", got)
	}
	// Non-loopback peer → ignore any forwarded-for, use the real socket IP.
	if got := peerIP(mkCtx("203.0.113.7:5555", "9.9.9.9")); got != "203.0.113.7" {
		t.Errorf("direct peer: peerIP = %q, want 203.0.113.7", got)
	}
	// Loopback peer, no XFF → falls back to loopback.
	if got := peerIP(mkCtx("127.0.0.1:5555", "")); got != "127.0.0.1" {
		t.Errorf("loopback peer no xff: peerIP = %q, want 127.0.0.1", got)
	}
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }
