package api

import (
	"bytes"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	webp "github.com/gen2brain/webp"
)

// A WebP source within the size cap goes out as it came: no decode, no
// re-encode.
func TestImageProxy_WebPSourcePassesThrough(t *testing.T) {
	withPlainImgClient(t)
	img := image.NewNRGBA(image.Rect(0, 0, 46, 69))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	img.Set(0, 0, color.NRGBA{255, 0, 0, 255})
	var src bytes.Buffer
	if err := webp.Encode(&src, img, webp.Options{Quality: 50}); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/webp")
		w.Write(src.Bytes()) //nolint:errcheck
	}))
	defer srv.Close()

	u := srv.URL + "/poster_460.webp?" + strconv.FormatInt(time.Now().UnixNano(), 36)
	rec := httptest.NewRecorder()
	ImageProxy(rec, imgReq(u))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(rec.Body.Bytes(), src.Bytes()) {
		t.Fatalf("re-encoded: %d bytes out, %d in", rec.Body.Len(), src.Len())
	}
}
