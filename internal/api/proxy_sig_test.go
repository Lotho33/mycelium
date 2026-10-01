package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mycelium/internal/core"
)

// requireProxySig is the gate every /proxy/* handler calls first: reject a
// request whose query has no valid HMAC, pass a correctly-signed one.
func TestRequireProxySig(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)

	old := proxyAllowUnsigned
	proxyAllowUnsigned = false
	defer func() { proxyAllowUnsigned = old }()

	base := "http://box.local/proxy/segment.ts?data=aHR0cHM&origin=&cookies=&vpn=0"

	// unsigned -> stop, 403
	rec := httptest.NewRecorder()
	if !requireProxySig(rec, httptest.NewRequest(http.MethodGet, base, nil)) {
		t.Fatal("unsigned request was not stopped")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unsigned request: status %d, want 403", rec.Code)
	}

	// tampered sig -> stop
	rec = httptest.NewRecorder()
	if !requireProxySig(rec, httptest.NewRequest(http.MethodGet, base+"&sig=deadbeef", nil)) {
		t.Fatal("request with a bogus sig was not stopped")
	}

	// correctly signed -> pass
	rec = httptest.NewRecorder()
	if requireProxySig(rec, httptest.NewRequest(http.MethodGet, core.AppendProxySig(base), nil)) {
		t.Fatalf("correctly signed request was stopped (%d %s)", rec.Code, rec.Body.String())
	}

	// escape hatch
	proxyAllowUnsigned = true
	rec = httptest.NewRecorder()
	if requireProxySig(rec, httptest.NewRequest(http.MethodGet, base, nil)) {
		t.Fatal("MYCELIUM_PROXY_ALLOW_UNSIGNED did not bypass the check")
	}
}

// A signed URL replayed with another uid and the same sig is rejected, so
// nobody can move a session onto another profile.
func TestRequireProxySig_UIDTamperRejected(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)

	old := proxyAllowUnsigned
	proxyAllowUnsigned = false
	defer func() { proxyAllowUnsigned = old }()

	signed := core.AppendProxySig("http://box.local/proxy/segment.ts?data=aHR0cHM&origin=&cookies=&vpn=0&uid=profile-A")

	// Sanity check: the request as originally signed must pass.
	rec := httptest.NewRecorder()
	if requireProxySig(rec, httptest.NewRequest(http.MethodGet, signed, nil)) {
		t.Fatalf("correctly signed uid=profile-A request was stopped (%d %s)", rec.Code, rec.Body.String())
	}

	tampered := strings.Replace(signed, "uid=profile-A", "uid=profile-B", 1)
	rec = httptest.NewRecorder()
	if !requireProxySig(rec, httptest.NewRequest(http.MethodGet, tampered, nil)) {
		t.Fatal("request with uid swapped to another profile (same sig) was NOT stopped")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("uid-tampered request: status %d, want 403", rec.Code)
	}
}
