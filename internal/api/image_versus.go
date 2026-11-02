package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	webp "github.com/gen2brain/webp"
	xdraw "golang.org/x/image/draw"

	"mycelium/internal/core"
)

// ─────────────────────────────────────────────────────────────────────────────
// Versus cover — GET /img/versus?h=<base64url URL>&a=<base64url URL>
// ─────────────────────────────────────────────────────────────────────────────
//
// A 16:9 cover built from two badges (home on the left, away on the right)
// for events that have no artwork of their own: each half is tinted with its
// badge's average colour, split by a slanted edge. Plugins get the URL from
// mycelium.image.versus_url (a server-relative "mycelium:/" URL, made
// absolute per client). Same guards as /img: SSRF-guarded fetch, source size
// caps, the conversion semaphore and the disk cache.

const (
	versusW        = 960
	versusH        = 540
	versusBadgeBox = 300 // each badge fits this square
	// portrait images (cut-out photos): this tall, standing on the bottom edge
	versusFigureH    = 470
	versusFigureMaxW = 420
	versusVersion    = "v4"
)

// ImageVersus serves the versus cover of h and a (see the file comment).
func ImageVersus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	home, away := imgB64Decode(q.Get("h")), imgB64Decode(q.Get("a"))
	for _, u := range []string{home, away} {
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" || strings.Contains(strings.ToLower(pu.Path), ".svg") {
			http.Error(w, "bad h/a", http.StatusBadRequest)
			return
		}
	}

	sum := sha256.Sum256([]byte("versus|" + versusVersion + "|" + home + "|" + away))
	cacheFile := filepath.Join(imageCacheDir(), hex.EncodeToString(sum[:])+".webp")
	if imgServeCached(w, r, cacheFile, home) {
		return
	}

	imgInflightMu.Lock()
	if f, ok := imgInflight[cacheFile]; ok {
		imgInflightMu.Unlock()
		select {
		case <-f.done:
		case <-r.Context().Done():
			return
		}
		if f.out != nil {
			imgServeHeaders(w)
			w.Header().Set("Content-Length", strconv.Itoa(len(f.out)))
			_, _ = w.Write(f.out)
			return
		}
		http.Error(w, "image unavailable", http.StatusBadGateway)
		return
	}
	flight := &imgFlight{done: make(chan struct{})}
	imgInflight[cacheFile] = flight
	imgInflightMu.Unlock()
	defer func() {
		imgInflightMu.Lock()
		delete(imgInflight, cacheFile)
		imgInflightMu.Unlock()
		close(flight.done)
	}()

	select {
	case imgConvSem <- struct{}{}:
		defer func() { <-imgConvSem }()
	case <-time.After(imgQueueWait):
		w.Header().Set("Retry-After", "2")
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	case <-r.Context().Done():
		return
	}

	hImg, err := imgFetchDecode(r.Context(), home)
	if err == nil {
		var aImg image.Image
		if aImg, err = imgFetchDecode(r.Context(), away); err == nil {
			var out []byte
			if out, err = versusEncode(hImg, aImg); err == nil {
				tmp := cacheFile + ".tmp" + strconv.FormatInt(time.Now().UnixNano(), 36)
				if e := os.WriteFile(tmp, out, 0o644); e == nil {
					_ = os.Rename(tmp, cacheFile)
					core.SafeGo("api/img-cache-sweep", imgCacheSweep)
				} else {
					_ = os.Remove(tmp)
				}
				flight.out = out
				imgServeHeaders(w)
				w.Header().Set("Content-Length", strconv.Itoa(len(out)))
				_, _ = w.Write(out)
				return
			}
		}
	}
	log.Printf("[img] versus failed (%v): %s vs %s", core.RedactURLError(err), imgShortURL(home), imgShortURL(away))
	http.Error(w, "image unavailable", http.StatusBadGateway)
}

// imgFetchDecode downloads and decodes one source image under the /img caps.
func imgFetchDecode(ctx context.Context, upstream string) (image.Image, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", proxyUserAgent)
	req.Header.Set("Accept", "image/webp,image/png,image/jpeg,*/*")
	resp, err := imgClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, imgSrcReadLimit))
	if err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > imgMaxSourcePixels {
		return nil, fmt.Errorf("source too large: %dx%d", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

// versusEncode draws the cover and encodes it as WebP.
func versusEncode(home, away image.Image) ([]byte, error) {
	dst := versusDraw(home, away)
	var b bytes.Buffer
	if err := webp.Encode(&b, dst, webp.Options{Quality: imgWebPQuality, Method: 4}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func versusDraw(home, away image.Image) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, versusW, versusH))
	hc, ac := badgeTint(home), badgeTint(away)
	base := color.NRGBA{18, 18, 22, 255}
	for y := 0; y < versusH; y++ {
		// The split slants from x=540 at the top to x=420 at the bottom.
		edge := 540 - 120*y/versusH
		shade := 1 - 0.45*float64(y)/versusH // darker towards the bottom
		for x := 0; x < versusW; x++ {
			// Soft edge: blend the two tints over 8 px.
			t := float64(x-edge+4) / 8
			if t < 0 {
				t = 0
			} else if t > 1 {
				t = 1
			}
			// Tint fades towards the outer borders.
			var fade float64
			if x < edge {
				fade = 0.55 + 0.45*float64(x)/float64(edge)
			} else {
				fade = 0.55 + 0.45*float64(versusW-x)/float64(versusW-edge)
			}
			mix := func(c1, c2, b uint8) uint8 {
				v := (float64(c1)*(1-t) + float64(c2)*t) * 0.42 * fade
				return uint8(min(255, (float64(b)+v)*shade))
			}
			dst.SetNRGBA(x, y, color.NRGBA{mix(hc.R, ac.R, base.R), mix(hc.G, ac.G, base.G), mix(hc.B, ac.B, base.B), 255})
		}
	}
	versusPlace(dst, home, versusW/4+10, versusH/2)
	versusPlace(dst, away, 3*versusW/4-10, versusH/2)
	return dst
}

// versusPlace scales src to fit versusBadgeBox (aspect kept) centred on
// cx,cy. A cut-out photo (a player, see isCutOut) is drawn larger and
// standing on the bottom edge instead, so a half-length figure doesn't
// float mid-card.
func versusPlace(dst *image.NRGBA, src image.Image, cx, cy int) {
	sb := opaqueBounds(src)
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 {
		return
	}
	var r image.Rectangle
	if isCutOut(src, sb) {
		h := versusFigureH
		w := h * sw / sh
		if w > versusFigureMaxW {
			w = versusFigureMaxW
			h = w * sh / sw
		}
		r = image.Rect(cx-w/2, versusH-h, cx-w/2+w, versusH)
	} else {
		w, h := versusBadgeBox, versusBadgeBox
		if sw > sh {
			h = versusBadgeBox * sh / sw
		} else {
			w = versusBadgeBox * sw / sh
		}
		r = image.Rect(cx-w/2, cy-h/2, cx-w/2+w, cy-h/2+h)
	}
	w, h := r.Dx(), r.Dy()
	scaled := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, sb, xdraw.Over, nil)
	stddraw.Draw(dst, r, scaled, image.Point{}, stddraw.Over)
}

// badgeTint is the average colour of src's opaque pixels, lifted when too
// dark to show on the dark background (grey for an empty or black badge).
func badgeTint(src image.Image) color.NRGBA {
	b := src.Bounds()
	step := max(1, max(b.Dx(), b.Dy())/64) // sample a ~64x64 grid
	var r, g, bl, n float64
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			c := color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
			if c.A < 128 {
				continue
			}
			r, g, bl, n = r+float64(c.R), g+float64(c.G), bl+float64(c.B), n+1
		}
	}
	if n == 0 {
		return color.NRGBA{110, 110, 120, 255}
	}
	r, g, bl = r/n, g/n, bl/n
	if l := 0.299*r + 0.587*g + 0.114*bl; l < 90 {
		k := 90 / max(l, 1)
		r, g, bl = min(255, r*k+20), min(255, g*k+20), min(255, bl*k+20)
	}
	return color.NRGBA{uint8(r), uint8(g), uint8(bl), 255}
}

// opaqueBounds is src's bounds trimmed of fully transparent borders (cut-out
// photos and many badges carry wide empty margins); the full bounds when
// nothing is opaque.
func opaqueBounds(src image.Image) image.Rectangle {
	b := src.Bounds()
	minX, minY, maxX, maxY := b.Max.X, b.Max.Y, b.Min.X-1, b.Min.Y-1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := src.At(x, y).RGBA(); a > 0x1000 {
				minX, maxX = min(minX, x), max(maxX, x)
				minY, maxY = min(minY, y), max(maxY, y)
			}
		}
	}
	if maxX < minX || maxY < minY {
		return b
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}

// isCutOut tells a cut-out photo from a badge: the photo is cut straight
// across the bottom of its canvas (a half-length figure), so the opaque
// area reaches the canvas' last row and covers most of its width there; a
// badge (shield, round crest) narrows towards its bottom and usually keeps a
// margin. ob is src's opaque bounds; an image with no transparency at all
// is never a cut-out.
func isCutOut(src image.Image, ob image.Rectangle) bool {
	b := src.Bounds()
	if ob == b || ob.Max.Y != b.Max.Y || ob.Dx() <= 0 {
		return false
	}
	y, n := b.Max.Y-1, 0
	for x := ob.Min.X; x < ob.Max.X; x++ {
		if _, _, _, a := src.At(x, y).RGBA(); a > 0x8000 {
			n++
		}
	}
	return n*10 >= ob.Dx()*6
}
