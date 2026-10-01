package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameOriginMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := SameOriginMiddleware(ok)
	cases := []struct {
		name, method, path, origin, referer string
		want                                int
	}{
		{"same origin", "POST", "/admin/settings/save", "http://192.168.1.10:8000", "", 204},
		{"other port same host", "POST", "/admin/settings/save", "http://192.168.1.10:8096", "", 403},
		{"other site", "POST", "/setup/save", "http://evil.example", "", 403},
		{"null origin", "POST", "/admin/settings/save", "null", "", 403},
		{"referer only, same", "DELETE", "/admin/downloads/x", "", "http://192.168.1.10:8000/admin/dashboard", 204},
		{"referer only, other", "POST", "/admin/pileus-web/update", "", "http://192.168.1.10:9000/", 403},
		{"no headers (script)", "POST", "/admin/settings/save", "", "", 204},
		{"GET not guarded", "GET", "/admin/dashboard", "http://evil.example", "", 204},
		{"grpc-web not guarded", "POST", "/grpc/mycelium.AuthService/ListProfiles", "http://evil.example", "", 204},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://192.168.1.10:8000"+c.path, nil)
		req.RemoteAddr = "192.168.1.20:5555"
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.referer != "" {
			req.Header.Set("Referer", c.referer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
	}
}
