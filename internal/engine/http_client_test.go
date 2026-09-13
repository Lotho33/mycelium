package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scrapingDial pulls the DialContext func out of the *http.Client returned by
// NewScrapingClient, so the SSRF guard can be exercised directly (same style
// as internal/core/ssrf_test.go) without depending on a real remote listener
// or the uTLS handshake on the TLS path.
func scrapingDial(t *testing.T, client *http.Client) func(ctx context.Context, network, addr string) (interface {
	Close() error
}, error) {
	t.Helper()
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatalf("NewScrapingClient transport has no DialContext (got %T)", client.Transport)
	}
	return func(ctx context.Context, network, addr string) (interface{ Close() error }, error) {
		return tr.DialContext(ctx, network, addr)
	}
}

// TestNewScrapingClientDirectBlocksInternalTargets verifies that
// NewScrapingClient("") — the default "direct egress" client every
// direct_egress plugin, and any plugin with no VPN configured, gets via
// mycelium.network.get/post/fetch (see lua_plugin.go's m.directClient) —
// refuses to dial loopback, the cloud metadata address, and a link-local
// address, instead of falling through to a bare net.Dialer.
func TestNewScrapingClientDirectBlocksInternalTargets(t *testing.T) {
	dial := scrapingDial(t, NewScrapingClient(""))

	for _, addr := range []string{"127.0.0.1:80", "169.254.169.254:80", "169.254.10.20:80", "[::1]:443"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := dial(ctx, "tcp", addr)
		cancel()
		if err == nil {
			conn.Close()
			t.Errorf("dial(%s) succeeded, want it blocked by the SSRF guard", addr)
			continue
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("dial(%s) error = %v, want it to mention the dial being blocked", addr, err)
		}
	}
}

// TestNewScrapingClientDirectEndToEndBlocksLoopback confirms the guard is
// actually wired into the client used for real requests (not just its
// Transport.DialContext in isolation): a full GET against a local
// httptest.Server — which listens on loopback — must fail closed.
func TestNewScrapingClientDirectEndToEndBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewScrapingClient("")
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected the direct scraping client to refuse loopback target %s, got success", srv.URL)
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error = %v, want it to mention the dial being blocked", err)
	}
}

// TestNewScrapingClientDirectAllowsPublicTarget checks the guard does not
// reject a normal public IP: whatever the dial's outcome (a sandboxed test
// runner may have no outbound network at all), the failure must not be the
// SSRF guard's own "blocked" error.
func TestNewScrapingClientDirectAllowsPublicTarget(t *testing.T) {
	dial := scrapingDial(t, NewScrapingClient(""))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "93.184.216.34:80") // example.com
	if err != nil {
		if strings.Contains(err.Error(), "blocked") {
			t.Fatalf("dial of a public IP was rejected by the SSRF guard: %v", err)
		}
		return // network-unreachable/timeout in a sandboxed runner is not a guard failure
	}
	conn.Close()
}

// TestNewScrapingClientProxyPathSkipsGuard documents and locks in that a
// caller-supplied proxy is NOT wrapped by the SSRF guard: the proxy itself
// resolves and dials the real destination (see core.ProxyDialer), so a local
// IP check here would inspect only the proxy's own address, not the target —
// same reasoning as newUTLSProxyTransport in internal/api/upstream_transport.go.
// A bogus proxy URL is used so the test needs no real listener; the assertion
// is only that failure comes from core.ProxyDialer's own scheme handling, not
// from the SSRF guard rejecting the (unrelated, non-loopback) proxy host.
func TestNewScrapingClientProxyPathSkipsGuard(t *testing.T) {
	dial := scrapingDial(t, NewScrapingClient("socks5://127.0.0.1:1"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The proxy itself is a loopback address. If the guard wrapped this path
	// too, this would fail with a "blocked" error before ever attempting the
	// SOCKS5 handshake; instead it must fail (no listener there) for an
	// unrelated reason such as connection refused.
	conn, err := dial(ctx, "tcp", "203.0.113.5:80") // TEST-NET-3, unroutable on purpose
	if err == nil {
		conn.Close()
	}
	if err != nil && strings.Contains(err.Error(), "blocked") {
		t.Fatalf("proxy path unexpectedly routed through the SSRF guard: %v", err)
	}
}
