package api

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

// fakeTS builds n MPEG-TS packets (sync byte + pseudo-random payload).
func fakeTS(n int) []byte {
	r := rand.New(rand.NewSource(1))
	b := make([]byte, n*188)
	r.Read(b)
	for i := 0; i < n; i++ {
		b[i*188] = 0x47
		// keep 0x47 out of the payload so a stray sync can't confuse checks
		for j := 1; j < 188; j++ {
			if b[i*188+j] == 0x47 {
				b[i*188+j] = 0x48
			}
		}
	}
	return b
}

func testImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 16), uint8(y * 16), 0x47, 0xFF})
		}
	}
	return img
}

func encodedImages(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	var b bytes.Buffer
	if err := png.Encode(&b, testImage()); err != nil {
		t.Fatal(err)
	}
	out["png"] = append([]byte(nil), b.Bytes()...)
	b.Reset()
	if err := jpeg.Encode(&b, testImage(), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	out["jpeg"] = append([]byte(nil), b.Bytes()...)
	b.Reset()
	if err := gif.Encode(&b, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	out["gif"] = append([]byte(nil), b.Bytes()...)
	return out
}

func TestImageWrapperLenParsesRealImages(t *testing.T) {
	for name, img := range encodedImages(t) {
		tail := []byte("payload-after-image")
		if got := imageWrapperLen(append(append([]byte(nil), img...), tail...)); got != len(img) {
			t.Errorf("%s: wrapper len = %d, want %d", name, got, len(img))
		}
		if got := imageWrapperLen(img[:len(img)/2]); got != 0 {
			t.Errorf("%s truncated: wrapper len = %d, want 0", name, got)
		}
	}
}

func TestNormalizeSegment(t *testing.T) {
	ts := fakeTS(40)
	imgs := encodedImages(t)
	cipher := bytes.Repeat([]byte{0xA5, 0x13}, 512) // 1024 bytes: 64 AES blocks

	cases := []struct {
		name      string
		body      []byte
		ct        string
		encrypted bool
		wantBody  []byte
		wantCT    string
		reject    bool
	}{
		{"plain TS", ts, "video/mp2t", false, ts, "video/mp2t", false},
		{"TS as octet-stream", ts, "application/octet-stream", false, ts, "video/MP2T", false},
		{"PNG wrapper as octet-stream", append(append([]byte(nil), imgs["png"]...), ts...), "application/octet-stream", false, ts, "video/MP2T", false},
		{"JPEG wrapper as image/jpeg", append(append([]byte(nil), imgs["jpeg"]...), ts...), "image/jpeg", false, ts, "video/MP2T", false},
		{"GIF wrapper as video/mp2t", append(append([]byte(nil), imgs["gif"]...), ts...), "video/mp2t", false, ts, "video/MP2T", false},
		{"junk prefix found by scan", append([]byte("XXXXjunkprefix"), ts...), "text/plain", false, ts, "video/MP2T", false},
		{"trailing junk trimmed", append(append([]byte(nil), ts...), imgs["png"]...), "video/mp2t", false, ts, "video/mp2t", false},
		{"HTML error as video/mp2t", []byte("<!DOCTYPE html><html>blocked</html>"), "video/mp2t", false, nil, "", true},
		{"JSON error as octet-stream", []byte(`{"error":"forbidden"}`), "application/octet-stream", false, nil, "", true},
		{"unknown bytes as text/html", bytes.Repeat([]byte{1, 2, 3}, 100), "text/html", false, nil, "", true},
		{"unknown bytes as video/x", bytes.Repeat([]byte{1, 2, 3}, 100), "video/x-whatever", false, bytes.Repeat([]byte{1, 2, 3}, 100), "video/x-whatever", false},
		{"webvtt mislabelled", []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhi"), "text/plain", false, []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhi"), "text/vtt", false},
		{"ciphertext untouched", cipher, "application/octet-stream", true, cipher, "video/MP2T", false},
		{"ciphertext behind PNG", append(append([]byte(nil), imgs["png"]...), cipher...), "image/png", true, cipher, "video/MP2T", false},
		{"ciphertext text page", []byte("<html>denied</html>"), "video/mp2t", true, nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := normalizeSegment(c.body, c.ct, "https://cdn.example/seg-1.ts", c.encrypted)
			if c.reject {
				if v.reject == "" {
					t.Fatalf("want reject, got ct=%q notes=%v", v.ct, v.notes)
				}
				return
			}
			if v.reject != "" {
				t.Fatalf("unexpected reject: %s", v.reject)
			}
			if !bytes.Equal(v.body, c.wantBody) {
				t.Fatalf("body: got %d bytes, want %d (notes %v)", len(v.body), len(c.wantBody), v.notes)
			}
			if !strings.EqualFold(v.ct, c.wantCT) {
				t.Fatalf("ct = %q, want %q", v.ct, c.wantCT)
			}
		})
	}
}

func TestTrimTSTailKeepsDataAfterACorruptPacket(t *testing.T) {
	ts := fakeTS(10)
	ts[5*188] = 0x00 // corrupt sync in the middle; valid packets follow
	v := segVerdict{body: ts, ct: "video/MP2T"}
	v.trimTSTail()
	if len(v.body) != len(ts) {
		t.Fatalf("trimmed to %d bytes, want the whole %d", len(v.body), len(ts))
	}
}
