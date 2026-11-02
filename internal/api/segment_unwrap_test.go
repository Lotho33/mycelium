package api

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/engine"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	zw.Write(b)
	zw.Close()
	return out.Bytes()
}

// pixelPNG packs data into the RGB samples of a w-wide PNG, opaque (the
// encoder writes RGB) or with varying alpha (RGBA).
func pixelPNG(t *testing.T, data []byte, w int, alpha bool) []byte {
	t.Helper()
	px := (len(data) + 2) / 3
	h := (px + w - 1) / w
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		var c [3]byte
		for k := 0; k < 3; k++ {
			if j := i*3 + k; j < len(data) {
				c[k] = data[j]
			}
		}
		a := byte(255)
		if alpha {
			a = byte(i)
		}
		img.SetNRGBA(i%w, i/w, color.NRGBA{c[0], c[1], c[2], a})
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

var pixelRule = core.SegmentUnwrap{Carrier: "png_pixels", Magic: "TESTMAGIC", Length: "u32be", Codec: "gzip"}

func pixelCarrier(t *testing.T, ts []byte, alpha bool) []byte {
	g := gz(t, ts)
	data := append([]byte(nil), pixelRule.Magic...)
	data = binary.BigEndian.AppendUint32(data, uint32(len(g)))
	data = append(data, g...)
	return pixelPNG(t, data, 512, alpha)
}

func TestSegmentUnwrap_PNGPixels(t *testing.T) {
	ts := fakeTS(400)
	for _, alpha := range []bool{false, true} {
		b := pixelCarrier(t, ts, alpha)
		if ct := b[25]; (alpha && ct != 6) || (!alpha && ct != 2) {
			t.Fatalf("alpha=%v: encoder wrote color type %d", alpha, ct)
		}
		got, _ := applySegmentUnwraps(b, []core.SegmentUnwrap{pixelRule})
		if !bytes.Equal(got, ts) {
			t.Fatalf("alpha=%v: payload mismatch (%d bytes)", alpha, len(got))
		}
	}
}

// Raw samples, no magic, no length: the payload runs to the last sample
// (here the TS fills the image exactly).
func TestSegmentUnwrap_PNGPixelsRawToEnd(t *testing.T) {
	ts := fakeTS(512 * 3) // 512 px * 3 B * 188 rows
	got, _ := applySegmentUnwraps(pixelPNG(t, ts, 512, false), []core.SegmentUnwrap{{Carrier: "png_pixels"}})
	if !bytes.Equal(got, ts) {
		t.Fatalf("payload mismatch (%d bytes)", len(got))
	}
}

// Without rules nothing is unpacked, and an ordinary image never matches.
func TestSegmentUnwrap_NoRulesNoMatch(t *testing.T) {
	b := pixelCarrier(t, fakeTS(50), false)
	if got, _ := applySegmentUnwraps(b, nil); got != nil {
		t.Fatal("unpacked without rules")
	}
	plain := pixelPNG(t, bytes.Repeat([]byte{1, 2, 3}, 2000), 64, false)
	if got, _ := applySegmentUnwraps(plain, []core.SegmentUnwrap{pixelRule}); got != nil {
		t.Fatalf("plain image yielded %d bytes", len(got))
	}
}

// Dimensions whose product overflows int64, a single huge row within the
// pixel budget, and a canvas past it: refused before any allocation.
func TestSegmentUnwrap_HostileDimensions(t *testing.T) {
	for _, d := range [][2]uint32{{0xFFFFFFFF, 0xFFFFFFFF}, {0xFFFFFFFF, 2}, {32 << 20, 1}, {1 << 14, 1 << 14}} {
		b := pixelCarrier(t, fakeTS(10), false)
		binary.BigEndian.PutUint32(b[16:20], d[0])
		binary.BigEndian.PutUint32(b[20:24], d[1])
		if got, _ := applySegmentUnwraps(b, []core.SegmentUnwrap{pixelRule}); got != nil {
			t.Fatalf("%dx%d decoded", d[0], d[1])
		}
	}
}

// A declared length past the segment limit is refused.
func TestSegmentUnwrap_HostileLength(t *testing.T) {
	data := append([]byte(pixelRule.Magic), binary.BigEndian.AppendUint32(nil, 0xFFFFFFF0)...)
	data = append(data, bytes.Repeat([]byte{7}, 3000)...)
	if got, _ := applySegmentUnwraps(pixelPNG(t, data, 64, false), []core.SegmentUnwrap{pixelRule}); got != nil {
		t.Fatal("oversized length accepted")
	}
}

func TestSegmentUnwrap_WebPChunk(t *testing.T) {
	ts := fakeTS(5)
	chunk := append([]byte("EXIF"), binary.LittleEndian.AppendUint32(nil, uint32(len(ts)))...)
	chunk = append(chunk, ts...)
	vp8 := append([]byte("VP8L"), binary.LittleEndian.AppendUint32(nil, 3)...)
	vp8 = append(vp8, 1, 2, 3, 0) // odd size + pad byte
	body := append(vp8, chunk...)
	b := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(4+len(body)))...)
	b = append(b, "WEBP"...)
	b = append(b, body...)
	got, _ := applySegmentUnwraps(b, []core.SegmentUnwrap{{Carrier: "webp_chunk", Chunk: "EXIF"}})
	if !bytes.Equal(got, ts) {
		t.Fatalf("payload mismatch (%d bytes)", len(got))
	}
	if got, _ := applySegmentUnwraps(b, []core.SegmentUnwrap{{Carrier: "webp_chunk", Chunk: "XMP "}}); got != nil {
		t.Fatal("matched a chunk that isn't there")
	}
}

func TestSegmentUnwrap_BodyMarker(t *testing.T) {
	ts := fakeTS(5)
	junk := []byte("\xff\xd8\xff some image bytes ")
	raw := append(append(append([]byte(nil), junk...), "RAWMARK"...), ts...)
	if got, _ := applySegmentUnwraps(raw, []core.SegmentUnwrap{{Carrier: "body", Magic: "RAWMARK"}}); !bytes.Equal(got, ts) {
		t.Fatal("raw marker payload mismatch")
	}
	zipped := append(append(append([]byte(nil), junk...), "GZMARK"...), gz(t, ts)...)
	rules := []core.SegmentUnwrap{{Carrier: "body", Magic: "RAWMARK"}, {Carrier: "body", Magic: "GZMARK", Codec: "gzip"}}
	if got, _ := applySegmentUnwraps(zipped, rules); !bytes.Equal(got, ts) {
		t.Fatal("gzip marker payload mismatch")
	}
	// Whatever a rule yields must be media.
	if got, _ := applySegmentUnwraps(append([]byte("RAWMARK"), "<html>"...), rules); got != nil {
		t.Fatal("non-media payload accepted")
	}
}

func TestNormalizeSegment_PluginRules(t *testing.T) {
	ts := fakeTS(300)
	body := pixelCarrier(t, ts, false)
	v := normalizeSegment(body, "image/png", "https://cdn.example/x.image", false, []core.SegmentUnwrap{pixelRule})
	if v.reject != "" || v.ct != "video/MP2T" || !bytes.Equal(v.body, ts) {
		t.Fatalf("verdict: reject=%q ct=%q body=%d notes=%v", v.reject, v.ct, len(v.body), v.notes)
	}
	// The same body without the plugin's rules is not unpacked.
	if v := normalizeSegment(body, "image/png", "https://cdn.example/x.image", false, nil); bytes.Equal(v.body, ts) {
		t.Fatal("unpacked without rules")
	}
}

// The playlist proxy tags every rewritten URL with the plugin (sp) only when
// that plugin declares rules, and the segment proxy applies them.
func TestProxy_SegmentUnwrapRulesTravelWithURLs(t *testing.T) {
	core.SetProxySignKey([]byte("unwrap-test"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)
	useLoopbackSegmentClient(t)

	dir := t.TempDir()
	manifest := "id: t.unwrap\nname: T\ndirect_egress: true\nentrypoints:\n  probe: probe\n" +
		"segments:\n  unwrap:\n    - carrier: png_pixels\n      magic: TESTMAGIC\n      length: u32be\n      codec: gzip\n" +
		"    - carrier: nonsense\n"
	os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644)
	os.WriteFile(filepath.Join(dir, "init.lua"), []byte("function probe() return {} end"), 0o644)
	if err := engine.LuaPlugins.LoadPlugin(dir); err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin("t.unwrap") })
	if n := len(engine.LuaPlugins.SegmentUnwrap("t.unwrap")); n != 1 {
		t.Fatalf("%d rules kept, want 1 (the invalid one dropped)", n)
	}

	ts := fakeTS(200)
	carrier := pixelCarrier(t, ts, false)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg1.image\n#EXT-X-ENDLIST\n")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(carrier)
	}))
	defer up.Close()

	playlist := func(sid string) string {
		rec := httptest.NewRecorder()
		ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, signedPlaylistURL(up.URL+"/v.m3u8", "&sid="+b64url(sid)), nil))
		return rec.Body.String()
	}
	if body := playlist("other\x00s"); strings.Contains(body, "sp=") {
		t.Fatalf("plugin without rules got sp:\n%s", body)
	}
	body := playlist("t.unwrap\x00s")
	var segURL string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "/segment.ts?") {
			segURL = line
		}
	}
	if !strings.Contains(segURL, "sp=t.unwrap") {
		t.Fatalf("segment URL without sp:\n%s", body)
	}
	rec := httptest.NewRecorder()
	ProxySegment(rec, httptest.NewRequest(http.MethodGet, segURL[strings.Index(segURL, "/proxy/"):], nil))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), ts) {
		t.Fatalf("segment: status %d, %d bytes (want the %d-byte TS)", rec.Code, rec.Body.Len(), len(ts))
	}
}
