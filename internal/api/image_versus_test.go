package api

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	webp "github.com/gen2brain/webp"
)

func solidPNG(t *testing.T, c color.NRGBA, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		img.SetNRGBA(i%w, i/w, c)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func versusReq(h, a string) *http.Request {
	enc := base64.RawURLEncoding.EncodeToString
	return httptest.NewRequest(http.MethodGet, "/img/versus?h="+enc([]byte(h))+"&a="+enc([]byte(a)), nil)
}

func TestImageVersus_ComposesBothBadges(t *testing.T) {
	withPlainImgClient(t)
	red := solidPNG(t, color.NRGBA{220, 30, 30, 255}, 200, 150)
	blue := solidPNG(t, color.NRGBA{30, 60, 220, 255}, 120, 240)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if r.URL.Path == "/red.png" {
			w.Write(red) //nolint:errcheck
			return
		}
		w.Write(blue) //nolint:errcheck
	}))
	defer srv.Close()

	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	rec := httptest.NewRecorder()
	ImageVersus(rec, versusReq(srv.URL+"/red.png?"+nonce, srv.URL+"/blue.png?"+nonce))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	img, err := webp.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != versusW || b.Dy() != versusH {
		t.Fatalf("size %v", b)
	}
	at := func(x, y int) color.NRGBA { return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA) }
	// Badge centres (lossy WebP: loose checks).
	if c := at(versusW/4+10, versusH/2); c.R < 150 || c.B > 90 {
		t.Fatalf("home badge pixel %v", c)
	}
	if c := at(3*versusW/4-10, versusH/2); c.B < 150 || c.R > 90 {
		t.Fatalf("away badge pixel %v", c)
	}
	// Background halves carry each badge's tint.
	if l, r := at(20, 40), at(versusW-20, 40); l.R <= l.B || r.B <= r.R {
		t.Fatalf("tints: left %v right %v", l, r)
	}
}

func TestImageVersus_BadRequests(t *testing.T) {
	for _, c := range []struct{ h, a string }{
		{"", "https://x/b.png"},
		{"ftp://x/a.png", "https://x/b.png"},
		{"https://x/a.svg", "https://x/b.png"},
	} {
		rec := httptest.NewRecorder()
		ImageVersus(rec, versusReq(c.h, c.a))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q/%q: status %d", c.h, c.a, rec.Code)
		}
	}
}

func TestImageVersus_UpstreamFailure(t *testing.T) {
	withPlainImgClient(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	rec := httptest.NewRecorder()
	ImageVersus(rec, versusReq(srv.URL+"/a.png?"+nonce, srv.URL+"/b.png?"+nonce))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
}

// A transparent or black badge still gets a visible tint.
func TestBadgeTint_DarkAndEmpty(t *testing.T) {
	empty := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	if c := badgeTint(empty); c.R < 90 {
		t.Fatalf("empty badge tint %v", c)
	}
	black := image.NewNRGBA(image.Rect(0, 0, 10, 10))
	for i := range black.Pix {
		if i%4 == 3 {
			black.Pix[i] = 255
		}
	}
	if c := badgeTint(black); int(c.R)+int(c.G)+int(c.B) < 60 {
		t.Fatalf("black badge tint %v", c)
	}
}

// A cut-out (opaque rows reaching the canvas bottom across most of the
// width) stands on the card's bottom edge; a shield-like badge with a
// margin stays centred; an image without transparency is never a cut-out.
func TestIsCutOut(t *testing.T) {
	figure := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 20; y < 100; y++ { // torso cut at the bottom
		for x := 25; x < 75; x++ {
			figure.SetNRGBA(x, y, color.NRGBA{200, 150, 120, 255})
		}
	}
	badge := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for y := 10; y < 90; y++ { // narrows towards the bottom, margin below
		for x := 10 + (y-10)/2; x < 90-(y-10)/2; x++ {
			badge.SetNRGBA(x, y, color.NRGBA{20, 60, 200, 255})
		}
	}
	opaque := image.NewNRGBA(image.Rect(0, 0, 50, 50))
	for i := range opaque.Pix {
		opaque.Pix[i] = 255
	}
	if ob := opaqueBounds(figure); ob != image.Rect(25, 20, 75, 100) || !isCutOut(figure, ob) {
		t.Fatalf("figure: bounds %v cut-out %v", ob, isCutOut(figure, ob))
	}
	if ob := opaqueBounds(badge); isCutOut(badge, ob) {
		t.Fatal("badge taken for a cut-out")
	}
	if isCutOut(opaque, opaqueBounds(opaque)) {
		t.Fatal("opaque image taken for a cut-out")
	}

	dst := versusDraw(figure, badge)
	at := func(x, y int) color.NRGBA { return dst.NRGBAAt(x, y) }
	// The figure reaches the bottom of the card; the badge doesn't.
	if c := at(versusW/4+10, versusH-2); c.R < 150 {
		t.Fatalf("figure not on the bottom edge: %v", c)
	}
	if c := at(3*versusW/4-10, versusH-2); c.B > 150 {
		t.Fatalf("badge reaches the bottom edge: %v", c)
	}
}
