package api

import (
	"net/http"
	"net/http/httptest"
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
