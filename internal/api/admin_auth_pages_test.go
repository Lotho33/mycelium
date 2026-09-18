package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsSecureRequest_DirectTLS covers the preexisting behaviour: a direct
// HTTPS connection (r.TLS != nil) is secure regardless of any header.
func TestIsSecureRequest_DirectTLS(t *testing.T) {
	r := httptest.NewRequest("GET", "/admin/login", nil)
	r.RemoteAddr = "203.0.113.5:54321" // untrusted, but irrelevant: r.TLS wins
	r.TLS = &tls.ConnectionState{}

	if !isSecureRequest(r) {
		t.Fatal("direct TLS connection must be treated as secure")
	}
}

// TestIsSecureRequest_UntrustedForwardedProtoNotHonoured is the bug's other
// edge: a direct plain-HTTP client that isn't behind any (trusted) proxy
// must not be able to fake its way to Secure=true by injecting
// X-Forwarded-Proto itself — that header is only meaningful coming from a
// proxy mycelium actually trusts.
func TestIsSecureRequest_UntrustedForwardedProtoNotHonoured(t *testing.T) {
	r := httptest.NewRequest("GET", "/admin/login", nil)
	r.RemoteAddr = "192.168.1.50:12345" // LAN, not in the default trust list
	r.Header.Set("X-Forwarded-Proto", "https")

	if isSecureRequest(r) {
		t.Fatal("X-Forwarded-Proto from an untrusted direct peer must not mark the request secure")
	}
}

// TestIsSecureRequest_TrustedProxyForwardedProtoHonoured is the fix itself:
// a TLS-terminating reverse proxy on loopback (trusted by default, same as
// realIP()'s X-Forwarded-For handling in ratelimit.go) talks plain HTTP to
// mycelium, so r.TLS is nil — but its X-Forwarded-Proto: https must still
// mark the request secure so the admin session cookie gets Secure=true.
func TestIsSecureRequest_TrustedProxyForwardedProtoHonoured(t *testing.T) {
	r := httptest.NewRequest("GET", "/admin/login", nil)
	r.RemoteAddr = "127.0.0.1:12345" // loopback: trusted by default
	r.Header.Set("X-Forwarded-Proto", "https")

	if !isSecureRequest(r) {
		t.Fatal("X-Forwarded-Proto: https from a trusted (loopback) proxy must mark the request secure")
	}
}

// TestIsSecureRequest_TrustedProxyPlainHTTPForwardedProtoNotSecure ensures a
// trusted proxy that itself terminates plain HTTP (X-Forwarded-Proto: http,
// or the header simply absent) does not spuriously flip Secure on.
func TestIsSecureRequest_TrustedProxyPlainHTTPForwardedProtoNotSecure(t *testing.T) {
	r := httptest.NewRequest("GET", "/admin/login", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Forwarded-Proto", "http")

	if isSecureRequest(r) {
		t.Fatal("X-Forwarded-Proto: http from a trusted proxy must not mark the request secure")
	}
}

// TestIsSecureRequest_EnvOverrideTrustsOnlyConfiguredCIDR exercises the
// MYCELIUM_TRUSTED_PROXY_CIDRS opt-in path (same mechanism realIP() uses):
// once configured, only that CIDR is trusted for X-Forwarded-Proto, and
// loopback reverts to untrusted.
func TestIsSecureRequest_EnvOverrideTrustsOnlyConfiguredCIDR(t *testing.T) {
	withTrustedProxyNetworks(t, "10.0.0.5/32")

	trusted := httptest.NewRequest("GET", "/admin/login", nil)
	trusted.RemoteAddr = "10.0.0.5:9999"
	trusted.Header.Set("X-Forwarded-Proto", "https")
	if !isSecureRequest(trusted) {
		t.Fatal("configured trusted proxy CIDR must have its X-Forwarded-Proto honoured")
	}

	noLongerTrusted := httptest.NewRequest("GET", "/admin/login", nil)
	noLongerTrusted.RemoteAddr = "127.0.0.1:12345"
	noLongerTrusted.Header.Set("X-Forwarded-Proto", "https")
	if isSecureRequest(noLongerTrusted) {
		t.Fatal("loopback must no longer be trusted once MYCELIUM_TRUSTED_PROXY_CIDRS replaces the default list")
	}
}

// TestSetAdminSessionCookie_SecureFlag exercises setAdminSessionCookie end
// to end for the three scenarios above, checking the actual Set-Cookie
// header rather than the isSecureRequest helper directly.
func TestSetAdminSessionCookie_SecureFlag(t *testing.T) {
	SetAdminSessionKey([]byte("test-master-secret"))
	defer SetAdminSessionKey(nil)

	cookieFor := func(r *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range r.Result().Cookies() {
			if c.Name == adminSessionCookie {
				return c
			}
		}
		return nil
	}

	t.Run("direct TLS", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/admin/login", nil)
		r.RemoteAddr = "203.0.113.5:54321"
		r.TLS = &tls.ConnectionState{}
		w := httptest.NewRecorder()
		setAdminSessionCookie(w, r)
		c := cookieFor(w)
		if c == nil || !c.Secure {
			t.Fatal("direct HTTPS request must set Secure=true on the admin session cookie")
		}
	})

	t.Run("untrusted plain HTTP with spoofed header", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/admin/login", nil)
		r.RemoteAddr = "192.168.1.50:12345"
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		setAdminSessionCookie(w, r)
		c := cookieFor(w)
		if c == nil || c.Secure {
			t.Fatal("spoofed X-Forwarded-Proto from an untrusted IP must not set Secure=true")
		}
	})

	t.Run("trusted loopback proxy", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/admin/login", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		setAdminSessionCookie(w, r)
		c := cookieFor(w)
		if c == nil || !c.Secure {
			t.Fatal("X-Forwarded-Proto: https from a trusted loopback proxy must set Secure=true")
		}
	})
}
