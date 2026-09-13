package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withTrustedProxyNetworks temporarily swaps the package-level trusted proxy
// list for the duration of a test, restoring the previous value afterwards.
// trustedProxyNetworks is populated once by init() from the
// MYCELIUM_TRUSTED_PROXY_CIDRS env var, so tests rebuild it via
// loadTrustedProxyNetworks (a pure function) instead of relying on process
// startup order.
func withTrustedProxyNetworks(t *testing.T, envVal string) {
	t.Helper()
	old := trustedProxyNetworks
	trustedProxyNetworks = loadTrustedProxyNetworks(envVal)
	t.Cleanup(func() { trustedProxyNetworks = old })
}

func newReqWithXFF(remoteAddr, xff string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestRealIP_DefaultDoesNotTrustLANForXFF(t *testing.T) {
	// Default trust list (loopback-only) must be in effect.
	withTrustedProxyNetworks(t, "")

	r := newReqWithXFF("192.168.1.50:12345", "1.2.3.4")
	got := realIP(r)
	if got != "192.168.1.50" {
		t.Fatalf("realIP() = %q, want %q (LAN RemoteAddr must not trust injected XFF by default)", got, "192.168.1.50")
	}
}

func TestRealIP_DefaultTrustsLoopbackForXFF(t *testing.T) {
	withTrustedProxyNetworks(t, "")

	r := newReqWithXFF("127.0.0.1:12345", "1.2.3.4")
	got := realIP(r)
	if got != "1.2.3.4" {
		t.Fatalf("realIP() = %q, want %q (loopback RemoteAddr must keep trusting XFF)", got, "1.2.3.4")
	}
}

func TestRealIP_DefaultTrustsIPv6LoopbackForXFF(t *testing.T) {
	withTrustedProxyNetworks(t, "")

	r := newReqWithXFF("[::1]:12345", "1.2.3.4")
	got := realIP(r)
	if got != "1.2.3.4" {
		t.Fatalf("realIP() = %q, want %q (IPv6 loopback RemoteAddr must trust XFF)", got, "1.2.3.4")
	}
}

func TestRealIP_EnvOverrideTrustsOnlyConfiguredCIDR(t *testing.T) {
	withTrustedProxyNetworks(t, "10.0.0.5/32")

	// Exact configured IP: XFF must be trusted.
	r := newReqWithXFF("10.0.0.5:9999", "5.6.7.8")
	got := realIP(r)
	if got != "5.6.7.8" {
		t.Fatalf("realIP() = %q, want %q (configured proxy IP must trust XFF)", got, "5.6.7.8")
	}

	// Different IP in the same /8, previously trusted under the old
	// RFC1918-wide default: must NOT be trusted now that the override is a
	// narrow /32.
	r2 := newReqWithXFF("10.0.0.99:9999", "5.6.7.8")
	got2 := realIP(r2)
	if got2 != "10.0.0.99" {
		t.Fatalf("realIP() = %q, want %q (IP outside the configured /32 must not trust XFF)", got2, "10.0.0.99")
	}
}

func TestLoadTrustedProxyNetworks_MalformedFallsBackToDefault(t *testing.T) {
	nets := loadTrustedProxyNetworks("not-a-cidr, also-bad")
	if len(nets) != len(trustedProxyNets) {
		t.Fatalf("loadTrustedProxyNetworks with only malformed entries should fall back to the %d default nets, got %d", len(trustedProxyNets), len(nets))
	}
}

func TestLoadTrustedProxyNetworks_PartiallyMalformedKeepsValidOnes(t *testing.T) {
	nets := loadTrustedProxyNetworks("not-a-cidr, 172.16.5.5/32")
	if len(nets) != 1 {
		t.Fatalf("loadTrustedProxyNetworks should keep the one valid CIDR and skip the malformed one, got %d nets", len(nets))
	}
	if !nets[0].Contains(mustParseIP(t, "172.16.5.5")) {
		t.Fatalf("expected the surviving network to contain 172.16.5.5")
	}
}

func mustParseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("failed to parse IP %q", s)
	}
	return ip
}
