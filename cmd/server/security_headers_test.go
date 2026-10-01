package main

import (
	"io"
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

func TestBodyLimitMiddleware(t *testing.T) {
	read := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		}
	})
	h := bodyLimitMiddleware(read)

	big := strings.NewReader(strings.Repeat("x", 2<<20))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/settings/save", big))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("2 MiB on a JSON route: status %d, want 413", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/lua-plugins/upload", strings.NewReader(strings.Repeat("x", 2<<20))))
	if rec.Code != http.StatusOK {
		t.Fatalf("2 MiB plugin upload: status %d, want 200", rec.Code)
	}
}
