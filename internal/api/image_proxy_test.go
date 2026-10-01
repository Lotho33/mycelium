package api

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
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

// imgClient's SSRF guard blocks loopback, where httptest listens: these
// tests are about fetch/decode/encode, so use a plain client (the guard is
// tested in internal/core).
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

// A decode failure returns the fetched bytes too, so ImageProxy can serve
// them itself (see TestImageProxy_DecodeFailurePassesRawBytesThrough).
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

// /img shares the dashboard's origin: a non-image body must never be served
// inline.
func TestImageProxy_NonImageBodyIsNotServedInline(t *testing.T) {
	withPlainImgClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><script>fetch('/admin/settings')</script></html>`)) //nolint:errcheck
	}))
	defer srv.Close()

	u := base64.RawURLEncoding.EncodeToString([]byte(srv.URL + "/x.html"))
	req := httptest.NewRequest(http.MethodGet, "/img?u="+u, nil)
	rec := httptest.NewRecorder()
	ImageProxy(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("non-image upstream served inline (status 200, Content-Type %q)", rec.Header().Get("Content-Type"))
	}
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Fatal("upstream HTML echoed in the response body")
	}
}

// A tiny PNG whose IHDR declares a huge canvas must be refused before
// image.Decode allocates it (decompression bomb), and without raw bytes so
// the caller doesn't pass it through either.
func TestImgFetchEncode_RejectsDecompressionBomb(t *testing.T) {
	withPlainImgClient(t)
	bomb := tinyPNG(t)
	// IHDR data starts at offset 16 (8 signature + 4 length + 4 type).
	binary.BigEndian.PutUint32(bomb[16:], 40000)
	binary.BigEndian.PutUint32(bomb[20:], 40000)
	binary.BigEndian.PutUint32(bomb[29:], crc32.ChecksumIEEE(bomb[12:29]))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(bomb) //nolint:errcheck
	}))
	defer srv.Close()

	req := httptest.NewRequest(http.MethodGet, "/img", nil)
	_, _, _, raw, _, err := imgFetchEncode(req, srv.URL+"/bomb.png")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want a \"source too large\" rejection", err)
	}
	if raw != nil {
		t.Error("oversized source must not come back as raw bytes for passthrough")
	}
}

func TestImgSniffRasterType(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"png", tinyPNG(t), "image/png"},
		{"gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), "image/gif"},
		{"avif", []byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00"), "image/avif"},
		{"html", []byte("<html><body>x</body></html>"), ""},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`), ""},
		{"text", []byte("hello"), ""},
	}
	for _, c := range cases {
		if got := imgSniffRasterType(c.in); got != c.want {
			t.Errorf("%s: imgSniffRasterType = %q, want %q", c.name, got, c.want)
		}
	}
}
