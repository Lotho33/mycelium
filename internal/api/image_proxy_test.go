package api

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImgB64Decode(t *testing.T) {
	cases := map[string]string{
		"aHR0cHM6Ly9leC5jb20vYS5qcGc":  "https://ex.com/a.jpg", // raw url, no padding
		"aHR0cHM6Ly9leC5jb20vYS5qcGc=": "https://ex.com/a.jpg", // url, padded
		"":                             "",
		"not valid base64!!!":          "",
	}
	for in, want := range cases {
		if got := imgB64Decode(in); got != want {
			t.Errorf("imgB64Decode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImgShortURL(t *testing.T) {
	cases := []struct{ in, want string }{
		// scheme and query dropped, host+path kept
		{"https://cdn.example.com/poster.jpg?token=abc&exp=123", "cdn.example.com/poster.jpg"},
		{"https://cdn.example.com/poster.jpg", "cdn.example.com/poster.jpg"},
	}
	for _, c := range cases {
		if got := imgShortURL(c.in); got != c.want {
			t.Errorf("imgShortURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// long host+path gets capped at 96 chars total, ending in "..."
	long := "https://cdn.example.com/" + strings.Repeat("b", 150)
	got := imgShortURL(long)
	if len(got) != 96 {
		t.Errorf("imgShortURL didn't cap to 96 chars: len=%d (%q)", len(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("capped imgShortURL doesn't end in \"...\": %q", got)
	}
	if !strings.HasPrefix(got, "cdn.example.com/") {
		t.Errorf("capped imgShortURL lost the host prefix: %q", got)
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 1, color.RGBA{B: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return b.Bytes()
}

// imgClient carries the SSRF guard (core.GuardedDialContext) added for P1-2,
// which blocks loopback — exactly where httptest servers listen. Swap it for
// a plain client for these tests, which are about the fetch/decode/encode
// pipeline, not the SSRF guard (covered separately in internal/core).
func withPlainImgClient(t *testing.T) {
	t.Helper()
	orig := imgClient
	imgClient = &http.Client{Timeout: imgFetchTimeout}
	t.Cleanup(func() { imgClient = orig })
}

func TestImgFetchEncode_HappyPath(t *testing.T) {
	withPlainImgClient(t)
	pngBytes := tinyPNG(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes) //nolint:errcheck
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/img", nil)
	out, srcLen, srcFmt, raw, _, err := imgFetchEncode(req, srv.URL+"/poster.png")
	if err != nil {
		t.Fatalf("imgFetchEncode: %v", err)
	}
	if raw != nil {
		t.Error("rawBody should be nil on the happy path — only set alongside a decode error")
	}
	if srcFmt != "png" {
		t.Errorf("srcFmt = %q, want png", srcFmt)
	}
	if srcLen != len(pngBytes) {
		t.Errorf("srcLen = %d, want %d", srcLen, len(pngBytes))
	}
	if len(out) < 12 || string(out[0:4]) != "RIFF" || string(out[8:12]) != "WEBP" {
		n := len(out)
		if n > 12 {
			n = 12
		}
		t.Errorf("output doesn't look like a WebP (RIFF/WEBP header): % x", out[:n])
	}
}

func TestImgFetchEncode_UpstreamErrorStatus(t *testing.T) {
	withPlainImgClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/img", nil)
	if _, _, _, raw, _, err := imgFetchEncode(req, srv.URL+"/missing.png"); err == nil {
		t.Fatal("expected an error for a 404 upstream")
	} else if raw != nil {
		t.Error("rawBody should be nil when the upstream fetch itself failed — nothing was read")
	}
}

// Renamed from ...FailsClosed: since the CORS/black-poster fix (2026-09-14)
// a decode failure no longer means "give up" for imgFetchEncode's caller —
// the raw bytes it already fetched come back too so ImageProxy can serve
// them same-origin instead of 302-redirecting a browser client to a foreign
// host that likely doesn't set Access-Control-Allow-Origin. This test is
// about imgFetchEncode's own contract; TestImageProxy_DecodeFailurePassesRawBytesThrough
// below checks the actual HTTP response ImageProxy builds from it.
func TestImgFetchEncode_UnknownFormatReturnsRawBytes(t *testing.T) {
	withPlainImgClient(t)
	const body = "this is not an image"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte(body)) //nolint:errcheck
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/img", nil)
	_, srcLen, _, raw, rawCT, err := imgFetchEncode(req, srv.URL+"/garbage.png")
	if err == nil {
		t.Fatal("expected a decode error for a non-image body")
	}
	if srcLen == 0 {
		t.Error("srcLen should still report the bytes read even on a decode failure")
	}
	if string(raw) != body {
		t.Errorf("rawBody = %q, want %q — caller needs these to pass through", raw, body)
	}
	if rawCT != "image/png" {
		t.Errorf("rawContentType = %q, want the upstream's own Content-Type", rawCT)
	}
}
