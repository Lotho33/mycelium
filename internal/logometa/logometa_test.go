package logometa

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"sync/atomic"
	"testing"
	"time"
)

// pngHeader returns the start of a PNG declaring a w×h canvas (enough for
// DecodeConfig, nothing to decode).
func pngHeader(w, h uint32) []byte {
	ihdr := make([]byte, 0, 17)
	ihdr = append(ihdr, "IHDR"...)
	ihdr = binary.BigEndian.AppendUint32(ihdr, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	out := []byte("\x89PNG\r\n\x1a\n")
	out = binary.BigEndian.AppendUint32(out, 13)
	out = append(out, ihdr...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(ihdr))
}

// logo draws a 400×100 transparent image with the left part (share of the
// width) in a and the rest in b; nil colours stay transparent.
func logo(a, b color.Color, share float64) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 400, 100))
	split := int(400 * share)
	for y := 20; y < 80; y++ { // a band: the logo has transparent padding
		for x := 0; x < 400; x++ {
			c := b
			if x < split {
				c = a
			}
			if c != nil {
				img.Set(x, y, c)
			}
		}
	}
	return img
}

func fill(c color.Color) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 300, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 300; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

var (
	black   = color.NRGBA{0, 0, 0, 255}
	white   = color.NRGBA{255, 255, 255, 255}
	darkRed = color.NRGBA{110, 10, 10, 255}
	yellow  = color.NRGBA{250, 210, 40, 255}
)

func TestAnalyzeHalo(t *testing.T) {
	cases := []struct {
		name string
		img  image.Image
		halo string
	}{
		{"black wordmark", logo(black, black, 1), "light"},
		{"white wordmark", logo(white, white, 1), ""},
		{"dark red wordmark", logo(darkRed, darkRed, 1), "light"},
		{"yellow wordmark", logo(yellow, yellow, 1), ""},
		{"half black, half white", logo(black, white, 0.5), "light"},
		{"mostly white, a black detail", logo(black, white, 0.15), ""},
		{"black on its own white box", fill(black), ""},
		{"transparent", logo(nil, nil, 1), ""},
	}
	for _, c := range cases {
		m := Analyze(c.img)
		if m.Halo != c.halo {
			t.Errorf("%s: halo %q, want %q (meta %+v)", c.name, m.Halo, c.halo, m)
		}
	}
}

func TestAnalyzeValues(t *testing.T) {
	m := Analyze(logo(black, white, 0.5))
	if m.W != 400 || m.H != 100 {
		t.Fatalf("size %dx%d", m.W, m.H)
	}
	if m.Low != 0 || m.High != 1 {
		t.Errorf("low/high = %v/%v, want 0/1", m.Low, m.High)
	}
	if m.DarkShare < 0.45 || m.DarkShare > 0.55 || m.LightShare < 0.45 || m.LightShare > 0.55 {
		t.Errorf("shares dark %v light %v", m.DarkShare, m.LightShare)
	}
	if m.Coverage < 0.55 || m.Coverage > 0.65 { // the band is 60% of the height
		t.Errorf("coverage %v", m.Coverage)
	}
	if c := Analyze(logo(darkRed, darkRed, 1)).Color; c != 0x6e0a0a {
		t.Errorf("colour %06x, want 6e0a0a", c)
	}
}

func TestSourceURL(t *testing.T) {
	cases := map[string]string{
		"":                                       "",
		"mycelium:/img/versus?a=b":               "",
		"javascript:alert(1)":                    "",
		"https://example.com/logo.png":           "https://example.com/logo.png",
		"https://example.com/logo.png|800|200|1": "https://example.com/logo.png",
		"https://image.tmdb.org/t/p/original/abcDEF123.png":       "https://image.tmdb.org/t/p/w300/abcDEF123.png",
		"https://image.tmdb.org/t/p/original/abcDEF123.svg":       "https://image.tmdb.org/t/p/w300/abcDEF123.png",
		"https://image.tmdb.org/t/p/w500/abcDEF123.svg|800|133|0": "https://image.tmdb.org/t/p/w300/abcDEF123.png",
		"https://example.com/a|b.png":                             "https://example.com/a|b.png",
	}
	for in, want := range cases {
		if got := SourceURL(in); got != want {
			t.Errorf("SourceURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLookupQueuesThenAnswers(t *testing.T) {
	start()
	mu.Lock()
	entries = map[string]*entry{}
	pending = map[string]bool{}
	mu.Unlock()
	var calls atomic.Int32
	fetch = func(_ context.Context, u string) (image.Image, error) {
		calls.Add(1)
		if u == "https://example.com/broken.png" {
			return nil, errors.New("upstream 404")
		}
		return logo(black, black, 1), nil
	}
	defer func() { fetch = fetchImage }()

	const u = "https://example.com/black.png"
	if _, ok := Lookup(u); ok {
		t.Fatal("first lookup should miss")
	}
	Lookup(u) // a second miss while pending doesn't queue it again
	waitFor(t, func() bool { _, ok := Lookup(u); return ok })
	m, _ := Lookup(u)
	if m.Halo != "light" {
		t.Errorf("halo %q", m.Halo)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}

	// A failure is remembered: no new fetch on every response.
	Lookup("https://example.com/broken.png")
	waitFor(t, func() bool { return calls.Load() == 2 })
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if _, ok := Lookup("https://example.com/broken.png"); ok {
			t.Fatal("broken logo reported as measured")
		}
	}
	time.Sleep(20 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Errorf("fetched %d times, want 2", n)
	}
}

func TestDecodeRejectsUnsupportedAndHuge(t *testing.T) {
	if _, err := decode([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)); !errors.Is(err, errUnsupported) {
		t.Errorf("svg: %v", err)
	}
	// A PNG header declaring 20000×20000 is refused before decoding.
	hdr := pngHeader(20000, 20000)
	if _, err := decode(hdr); err == nil || errors.Is(err, errUnsupported) {
		t.Errorf("huge png: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
