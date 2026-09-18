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

// TestRequireProxySig_UIDTamperRejected is the end-to-end regression test for
// the cross-profile "continue watching" tampering bug: a request minted with
// uid=profile-A, then replayed with uid swapped to profile-B and the same
// `sig` left untouched (exactly what a caller holding a legitimately-signed
// /proxy/* URL — its own player already fetches it — plus another profile's
// id, trivially obtained via ListProfiles, could do), must be rejected with
// 403 rather than let a session/UpsertProgress write land under profile-B.
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
