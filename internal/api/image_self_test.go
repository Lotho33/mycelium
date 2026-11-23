package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// selfHost stands for the mycelium base a client resolved a mycelium: URL
// against: it must never be contacted.
func selfHost(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "should not be fetched", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func badgeServer(t *testing.T) *httptest.Server {
	t.Helper()
	red := solidPNG(t, color.NRGBA{220, 30, 30, 255}, 64, 64)
	blue := solidPNG(t, color.NRGBA{30, 60, 220, 255}, 64, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if r.URL.Path == "/red.png" {
			w.Write(red) //nolint:errcheck
			return
		}
		w.Write(blue) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	return srv
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func imgReq(upstream string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/img?u="+b64(upstream), nil)
}

func TestImageProxy_SelfVersusServedInProcess(t *testing.T) {
	withPlainImgClient(t)
	badges := badgeServer(t)
	self, selfHits := selfHost(t)

	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	home, away := badges.URL+"/red.png?"+nonce, badges.URL+"/blue.png?"+nonce
	inner := self.URL + "/img/versus?h=" + b64(home) + "&a=" + b64(away)

	rec := httptest.NewRecorder()
	ImageProxy(rec, imgReq(inner))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("status %d, type %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if n := selfHits.Load(); n != 0 {
		t.Fatalf("mycelium's own URL fetched over HTTP %d times", n)
	}

	// Same cache file as GET /img/versus with the same h/a.
	sum := sha256.Sum256([]byte("versus|" + versusVersion + "|" + home + "|" + away))
	cached, err := os.ReadFile(filepath.Join(imageCacheDir(), hex.EncodeToString(sum[:])+".webp"))
	if err != nil {
		t.Fatalf("versus cache file: %v", err)
	}
	if !bytes.Equal(cached, rec.Body.Bytes()) {
		t.Fatal("/img body differs from the versus cache file")
	}
	direct := httptest.NewRecorder()
	ImageVersus(direct, versusReq(home, away))
	if !bytes.Equal(direct.Body.Bytes(), rec.Body.Bytes()) {
		t.Fatal("/img/versus serves something else")
	}
}

func TestImageProxy_SelfURLTakesOneSemaphoreSlot(t *testing.T) {
	withPlainImgClient(t)
	badges := badgeServer(t)
	self, selfHits := selfHost(t)

	// All slots but one taken: the nested request must still get through.
	for i := 0; i < cap(imgConvSem)-1; i++ {
		imgConvSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(imgConvSem)-1; i++ {
			<-imgConvSem
		}
	}()

	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	versus := self.URL + "/img/versus?h=" + b64(badges.URL+"/red.png?"+nonce) + "&a=" + b64(badges.URL+"/blue.png?"+nonce)
	nested := self.URL + "/img?u=" + b64(versus)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		ImageProxy(rec, imgReq(nested))
		done <- rec
	}()
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(imgQueueWait / 2):
		t.Fatal("nested /img waited for a second semaphore slot")
	}
	if n := selfHits.Load(); n != 0 {
		t.Fatalf("mycelium's own URL fetched over HTTP %d times", n)
	}
}

func TestImageProxy_NestedImgServesInnermost(t *testing.T) {
	withPlainImgClient(t)
	png := tinyPNG(t)
	var extHits atomic.Int32
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extHits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(png) //nolint:errcheck
	}))
	defer ext.Close()
	self, selfHits := selfHost(t)

	external := ext.URL + "/poster.png?" + strconv.FormatInt(time.Now().UnixNano(), 36)
	nested := self.URL + "/img?u=" + b64(self.URL+"/img?u="+b64(external))

	rec := httptest.NewRecorder()
	ImageProxy(rec, imgReq(nested))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if extHits.Load() != 1 || selfHits.Load() != 0 {
		t.Fatalf("external hits %d, self hits %d", extHits.Load(), selfHits.Load())
	}

	// Cached under the innermost URL: /img of the external image is a hit.
	again := httptest.NewRecorder()
	ImageProxy(again, imgReq(external))
	if again.Code != http.StatusOK || extHits.Load() != 1 {
		t.Fatalf("status %d, external hits %d", again.Code, extHits.Load())
	}
}

func TestImageProxy_TooDeepNestingIsRejected(t *testing.T) {
	self, selfHits := selfHost(t)
	u := "https://example.com/poster.png"
	for i := 0; i <= imgMaxSelfNesting; i++ {
		u = self.URL + "/img?u=" + b64(u)
	}
	rec := httptest.NewRecorder()
	ImageProxy(rec, imgReq(u))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if selfHits.Load() != 0 {
		t.Fatal("self host contacted")
	}
}
