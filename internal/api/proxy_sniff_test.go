package api

import (
	"bytes"
	"io"
	"math/rand"
	"strings"
	"testing"
)

// tsPayload builds n MPEG-TS packets (sync byte at every 188-byte stride).
func tsPayload(n int) []byte {
	b := make([]byte, n*188)
	for i := 0; i < n; i++ {
		b[i*188] = 0x47
		b[i*188+1] = byte(i)
	}
	return b
}

func TestSniffMediaStart_LargeWrappers(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	noise := func(n int) []byte { // binary noise with no 0x47 runs: like a compressed image
		b := make([]byte, n)
		rng.Read(b)
		for i := range b {
			if b[i] == 0x47 {
				b[i] = 0x48
			}
		}
		return b
	}
	html := func(n int) []byte {
		return []byte("<!DOCTYPE html><html><body>free" + strings.Repeat("x", n))
	}
	for _, tc := range []struct {
		name    string
		wrapper []byte
	}{
		{"html small", html(6000)},
		{"html just under old 64KiB limit", html(64600)},
		{"html over old 64KiB limit", html(150_000)},
		{"image-sized noise", noise(700_000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			media := tsPayload(2000)
			body := append(append([]byte{}, tc.wrapper...), media...)
			buf, off, ct, err := sniffMediaStart(bytes.NewReader(body))
			if err != nil || off != len(tc.wrapper) || ct != "video/MP2T" {
				t.Fatalf("off=%d want %d ct=%q err=%v", off, len(tc.wrapper), ct, err)
			}
			if !bytes.Equal(buf[off:off+188*3], media[:188*3]) {
				t.Fatal("buffer at offset is not the media start")
			}
		})
	}
}

func TestSniffMediaStart_FMP4AfterWrapper(t *testing.T) {
	wrapper := []byte(strings.Repeat("<p>free</p>", 2000)) // ~22 KB, contains the word "free"
	box := append([]byte{0, 0, 0, 24}, []byte("styp")...)
	box = append(box, make([]byte, 16)...)
	_, off, ct, _ := sniffMediaStart(bytes.NewReader(append(append([]byte{}, wrapper...), box...)))
	if off != len(wrapper) || ct != "video/mp4" {
		t.Fatalf("off=%d want %d ct=%q", off, len(wrapper), ct)
	}
}

func TestSniffMediaStart_RealErrorPage(t *testing.T) {
	page := []byte("<html><body>Just a moment... free moof</body></html>")
	buf, off, _, err := sniffMediaStart(bytes.NewReader(page))
	if err != nil || off != -1 || !bytes.Equal(buf, page) {
		t.Fatalf("off=%d err=%v len=%d", off, err, len(buf))
	}
}

func TestSniffMediaStart_NoFalseSyncOnTwoBytes(t *testing.T) {
	// 0x47 at i and i+188 only (the old 2-sync rule) must not count as TS.
	b := make([]byte, 4000)
	b[100], b[288] = 0x47, 0x47
	if off := tsSyncOffset(b); off != -1 {
		t.Fatalf("tsSyncOffset = %d, want -1", off)
	}
}

func TestSniffMediaStart_ReaderRemainderIsPreserved(t *testing.T) {
	wrapper := bytes.Repeat([]byte("a"), 100_000)
	media := tsPayload(5000)
	r := bytes.NewReader(append(append([]byte{}, wrapper...), media...))
	head, off, _, _ := sniffMediaStart(r)
	rest, _ := io.ReadAll(r)
	got := append(head[off:], rest...)
	if !bytes.Equal(got, media) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(media))
	}
}
