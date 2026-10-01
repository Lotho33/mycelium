package pileus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A non-grpc-web POST must not be forwarded — the catch-all "/" mount relies on
// this to 404 anything that isn't an application/grpc-web request so no other
// route is shadowed.
func TestServeGRPCWeb_RejectsNonGRPCWeb(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/health", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	serveGRPCWeb(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServeGRPCWeb_PreflightEchoesRequestedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/grpc/mycelium.AuthService/Login", nil)
	req.Header.Set("Access-Control-Request-Headers", "x-http-host,x-http-scheme,authorization")
	rec := httptest.NewRecorder()

	serveGRPCWeb(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "x-http-host,x-http-scheme,authorization" {
		t.Fatalf("Allow-Headers = %q, want the echoed request headers", got)
	}
}

func TestServeGRPCWeb_PreflightDefaultAllowHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/grpc/mycelium.AuthService/Login", nil)
	rec := httptest.NewRecorder()

	serveGRPCWeb(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	// x-http-host / x-http-scheme must be in the static allow-list so a client
	// that doesn't send Access-Control-Request-Headers can still pass them.
	allow := rec.Header().Get("Access-Control-Allow-Headers")
	for _, h := range []string{"x-http-host", "x-http-scheme"} {
		if !strings.Contains(allow, h) {
			t.Fatalf("Allow-Headers %q missing %q", allow, h)
		}
	}
}
