package pileus

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// Clients that reach the server through its tailnet name/address get proxy
// URLs on that same name/address, even with a server_host override (a LAN
// or reverse-proxy address they may not reach from outside home).
func TestClientProxyBaseTailnet(t *testing.T) {
	t.Setenv("MYCELIUM_SERVER_HOST", "https://media.example.com")
	SetTailnetHostMatcher(func(h string) bool {
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		return strings.EqualFold(h, "mycelium.tail1234.ts.net") || h == "100.101.102.103"
	})
	t.Cleanup(func() { SetTailnetHostMatcher(nil) })

	cases := []struct {
		name       string
		md         metadata.MD
		local      string
		wantScheme string
		wantHost   string
	}{
		{"web app over https on the tailnet name",
			metadata.Pairs("x-http-host", "mycelium.tail1234.ts.net", "x-http-scheme", "https"), "",
			"https", "mycelium.tail1234.ts.net"},
		{"tailnet name with explicit port",
			metadata.Pairs("x-http-host", "mycelium.tail1234.ts.net:8000", "x-http-scheme", "http"), "",
			"http", "mycelium.tail1234.ts.net:8000"},
		{"native client dialing the tailscale IP",
			metadata.MD{}, "100.101.102.103:50051",
			"http", "100.101.102.103:8000"},
		{"LAN client keeps server_host",
			metadata.Pairs("x-http-host", "192.168.1.10:8000", "x-http-scheme", "http"), "",
			"https", "media.example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), c.md)
			if c.local != "" {
				ctx = peer.NewContext(ctx, &peer.Peer{Addr: fakeAddr("100.64.0.9:41000"), LocalAddr: fakeAddr(c.local)})
			}
			scheme, host := clientProxyBase(ctx)
			if scheme != c.wantScheme || host != c.wantHost {
				t.Fatalf("got %s://%s, want %s://%s", scheme, host, c.wantScheme, c.wantHost)
			}
		})
	}

	// Without remote access nothing changes: server_host wins.
	SetTailnetHostMatcher(nil)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-http-host", "mycelium.tail1234.ts.net", "x-http-scheme", "https"))
	if scheme, host := clientProxyBase(ctx); scheme+"://"+host != "https://media.example.com" {
		t.Fatalf("matcher off: got %s://%s", scheme, host)
	}
}
