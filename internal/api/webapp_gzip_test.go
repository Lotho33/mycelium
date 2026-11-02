package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newWebAppMux(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<html><base href="/"></html>`), 0644)
	os.WriteFile(filepath.Join(dir, "main.dart.js"), []byte(strings.Repeat("var a=1;", 4096)), 0644)
	os.WriteFile(filepath.Join(dir, "icon.png"), []byte("png"), 0644)
	mux := http.NewServeMux()
	serveWebApp(mux, "/app", dir)
	return mux, dir
}

func TestServeWebApp_GzipsScriptsWhenAccepted(t *testing.T) {
	mux, _ := newWebAppMux(t)
	req := httptest.NewRequest("GET", "/app/main.dart.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("code=%d encoding=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control=%q", cc)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("Content-Type=%q", rec.Header().Get("Content-Type"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if len(body) != 8*4096 {
		t.Fatalf("decompressed %d bytes", len(body))
	}
}

func TestServeWebApp_PlainWithoutAcceptEncoding(t *testing.T) {
	mux, _ := newWebAppMux(t)
	for _, ae := range []string{"", "br", "gzip;q=0"} {
		req := httptest.NewRequest("GET", "/app/main.dart.js", nil)
		req.Header.Set("Accept-Encoding", ae)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" || rec.Body.Len() != 8*4096 {
			t.Fatalf("ae=%q: encoding=%q len=%d", ae, rec.Header().Get("Content-Encoding"), rec.Body.Len())
		}
	}
}

func TestServeWebApp_RecompressesAfterUpdate(t *testing.T) {
	mux, dir := newWebAppMux(t)
	get := func() string {
		req := httptest.NewRequest("GET", "/app/main.dart.js", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(zr)
		return string(b)
	}
	get()
	os.WriteFile(filepath.Join(dir, "main.dart.js"), []byte(strings.Repeat("var b=2;", 2048)), 0644)
	if got := get(); !strings.HasPrefix(got, "var b=2;") || len(got) != 8*2048 {
		t.Fatalf("stale body after update: %d bytes", len(got))
	}
}

func TestServeWebApp_IndexAndBinariesNoCache(t *testing.T) {
	mux, _ := newWebAppMux(t)
	for _, p := range []string{"/app/", "/app/some/route", "/app/icon.png"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%s: code=%d cc=%q enc=%q", p, rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("Content-Encoding"))
		}
	}
}
