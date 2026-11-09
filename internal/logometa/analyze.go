// Package logometa measures the logo images plugins return (logo_url), so
// clients can tell whether a logo needs a halo to stay readable, without
// every plugin analysing logos itself.
//
// The measuring happens in the background, once per URL (see store.go): a
// response never waits for it.
package logometa

import (
	"image"
	"image/color"
	"math"
	"sort"

	xdraw "golang.org/x/image/draw"
)

// Meta is what the core knows about one logo image.
type Meta struct {
	W          int     `json:"w,omitempty"`
	H          int     `json:"h,omitempty"`
	Low        float32 `json:"lo,omitempty"` // luminance, 10th percentile of visible pixels
	Mid        float32 `json:"mi,omitempty"` // median
	High       float32 `json:"hi,omitempty"` // 90th percentile
	DarkShare  float32 `json:"ds,omitempty"` // share of visible pixels below darkLum
	LightShare float32 `json:"ls,omitempty"` // share above lightLum
	Coverage   float32 `json:"cv,omitempty"` // visible pixels / all pixels
	Color      uint32  `json:"c,omitempty"`  // dominant colour 0xRRGGBB
	Halo       string  `json:"ha,omitempty"` // "" or "light": for a dark background
}

const (
	// sampleWidth: logos are measured on a copy this wide (enough for the
	// statistics, cheap at any source size).
	sampleWidth = 160
	// visibleAlpha: pixels at least this opaque count as part of the logo.
	visibleAlpha = 128
	// darkLum: below it a pixel has under ~2.1:1 contrast on the near-black
	// backgrounds clients draw logos on, i.e. it barely shows.
	darkLum = 0.10
	// lightLum: above it a pixel reads as light.
	lightLum = 0.60
	// bodyLum: a median below this is under ~2:1 on a near-black background.
	// (WCAG asks 3:1 for large text, i.e. 0.16, but saturated reds around
	// 0.12 read fine on black and look worse with a white halo.)
	bodyLum = 0.10
	// darkShareHalo: a logo where at least this share is barely visible
	// (e.g. a black word next to a white one) gets a halo too.
	darkShareHalo = 0.35
	// opaqueCoverage: an image this covered has a background of its own
	// (a box, a plate): nothing to add.
	opaqueCoverage = 0.97
)

// srgbToLinear maps an 8-bit sRGB channel to linear light (WCAG).
var srgbToLinear = func() (t [256]float64) {
	for i := range t {
		c := float64(i) / 255
		if c <= 0.04045 {
			t[i] = c / 12.92
		} else {
			t[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return
}()

func luminance(r, g, b uint8) float64 {
	return 0.2126*srgbToLinear[r] + 0.7152*srgbToLinear[g] + 0.0722*srgbToLinear[b]
}

// Analyze measures img (see Meta).
func Analyze(img image.Image) Meta {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	m := Meta{W: w, H: h}
	if w <= 0 || h <= 0 {
		return m
	}
	sw, sh := w, h
	if sw > sampleWidth {
		sw = sampleWidth
		sh = max(1, h*sampleWidth/w)
	}
	s := image.NewNRGBA(image.Rect(0, 0, sw, sh))
	xdraw.ApproxBiLinear.Scale(s, s.Bounds(), img, b, xdraw.Src, nil)

	lums := make([]float64, 0, sw*sh)
	var dark, light int
	// Dominant colour: the fullest bucket of a 4-bit-per-channel histogram,
	// averaged over its pixels.
	type bucket struct{ n, r, g, b int }
	hist := map[uint16]*bucket{}
	for i := 0; i+3 < len(s.Pix); i += 4 {
		px := color.NRGBA{s.Pix[i], s.Pix[i+1], s.Pix[i+2], s.Pix[i+3]}
		if px.A < visibleAlpha {
			continue
		}
		l := luminance(px.R, px.G, px.B)
		lums = append(lums, l)
		if l < darkLum {
			dark++
		} else if l > lightLum {
			light++
		}
		k := uint16(px.R>>4)<<8 | uint16(px.G>>4)<<4 | uint16(px.B>>4)
		bk := hist[k]
		if bk == nil {
			bk = &bucket{}
			hist[k] = bk
		}
		bk.n++
		bk.r += int(px.R)
		bk.g += int(px.G)
		bk.b += int(px.B)
	}
	n := len(lums)
	if n == 0 {
		return m // fully transparent: nothing to say
	}
	sort.Float64s(lums)
	pct := func(p float64) float32 { return round3(lums[min(n-1, int(p*float64(n)))]) }
	m.Low, m.Mid, m.High = pct(0.10), pct(0.50), pct(0.90)
	m.DarkShare = round3(float64(dark) / float64(n))
	m.LightShare = round3(float64(light) / float64(n))
	m.Coverage = round3(float64(n) / float64(sw*sh))
	var best *bucket
	for _, bk := range hist {
		if best == nil || bk.n > best.n {
			best = bk
		}
	}
	m.Color = uint32(best.r/best.n)<<16 | uint32(best.g/best.n)<<8 | uint32(best.b/best.n)

	if m.Coverage < opaqueCoverage && (m.Mid < bodyLum || m.DarkShare >= darkShareHalo) {
		m.Halo = "light"
	}
	return m
}

func round3(v float64) float32 { return float32(math.Round(v*1000) / 1000) }
