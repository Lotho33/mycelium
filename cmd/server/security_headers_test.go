package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersMiddleware(t *testing.T) {
	h := securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, path := range []string{"/admin", "/img", "/plugin-icon/x", "/proxy/playlist.m3u8", "/app/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
			t.Errorf("%s: X-Frame-Options = %q, want SAMEORIGIN", path, got)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		thirdParty := path == "/img" || strings.HasPrefix(path, "/plugin-icon/") || strings.HasPrefix(path, "/proxy/")
		if thirdParty && !strings.Contains(csp, "sandbox") {
			t.Errorf("%s: CSP = %q, want a sandbox policy on third-party content", path, csp)
		}
		if !thirdParty && csp != "" {
			t.Errorf("%s: unexpected CSP %q on first-party page", path, csp)
		}
	}
}
