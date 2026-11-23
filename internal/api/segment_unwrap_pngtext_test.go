package api

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"testing"

	"mycelium/internal/core"
)

func pngChunk(typ string, data []byte) []byte {
	c := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	c = append(c, typ...)
	c = append(c, data...)
	return binary.BigEndian.AppendUint32(c, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

func zlibBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zlib.NewWriter(&out)
	zw.Write(b)
	zw.Close()
	return out.Bytes()
}

// textPNG builds a tiny PNG carrying text in iTXt chunks keyword+"0001"…,
// compressed or not, after a tEXt chunk that isn't part of the payload.
func textPNG(t *testing.T, keyword string, text []byte, parts int, compressed bool) []byte {
	t.Helper()
	b := []byte("\x89PNG\r\n\x1a\n")
	b = append(b, pngChunk("IHDR", []byte{0, 0, 0, 1, 0, 0, 0, 1, 8, 2, 0, 0, 0})...)
	b = append(b, pngChunk("IDAT", zlibBytes(t, []byte{0, 0, 0, 0}))...)
	b = append(b, pngChunk("tEXt", []byte("Software\x00an editor"))...)
	step := (len(text) + parts - 1) / parts
	for i := 0; i < parts; i++ {
		piece := text[min(i*step, len(text)):min((i+1)*step, len(text))]
		flag := byte(0)
		if compressed {
			flag, piece = 1, zlibBytes(t, piece)
		}
		d := append([]byte(fmt.Sprintf("%s%04d", keyword, i+1)), 0, flag, 0, 0, 0)
		b = append(b, pngChunk("iTXt", append(d, piece...))...)
	}
	return append(b, pngChunk("IEND", nil)...)
}

func TestSegmentUnwrap_PNGText(t *testing.T) {
	ts := fakeTS(40)
	b64 := []byte(base64.StdEncoding.EncodeToString(ts))
	rule := core.SegmentUnwrap{Carrier: "png_text", Chunk: "payload-", Codec: "base64"}
	if err := rule.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, compressed := range []bool{true, false} {
		b := textPNG(t, "payload-", b64, 7, compressed)
		got, how := applySegmentUnwraps(b, []core.SegmentUnwrap{rule})
		if !bytes.Equal(got, ts) {
			t.Fatalf("compressed=%v: payload mismatch (%d bytes)", compressed, len(got))
		}
		if how != "plugin rule png_text" {
			t.Fatalf("how = %q", how)
		}
	}
	// Another keyword prefix: nothing to take.
	b := textPNG(t, "payload-", b64, 3, true)
	if got, _ := applySegmentUnwraps(b, []core.SegmentUnwrap{{Carrier: "png_text", Chunk: "other-", Codec: "base64"}}); got != nil {
		t.Fatal("matched a keyword that isn't there")
	}
	// Not base64 / not media: ignored, never an error.
	junk := textPNG(t, "payload-", []byte("<html>not a segment</html>"), 1, false)
	if got, _ := applySegmentUnwraps(junk, []core.SegmentUnwrap{rule}); got != nil {
		t.Fatal("non-media text accepted")
	}
	if got, _ := applySegmentUnwraps([]byte("GIF89a"), []core.SegmentUnwrap{rule}); got != nil {
		t.Fatal("non-PNG accepted")
	}
}

func TestSegmentUnwrap_PNGTextBomb(t *testing.T) {
	// A small compressed chunk that inflates past what a segment may be.
	huge := bytes.Repeat([]byte("A"), segmentBufferLimit/3*4+64)
	b := textPNG(t, "payload-", huge, 1, true)
	if pngTextData(b, "payload-") != nil {
		t.Fatal("oversized text not refused")
	}
}

func TestSegmentUnwrapRule_PNGTextValidation(t *testing.T) {
	long := string(bytes.Repeat([]byte("k"), 80))
	if (core.SegmentUnwrap{Carrier: "png_text", Chunk: long}).Validate() == nil {
		t.Fatal("keyword prefix over 79 bytes accepted")
	}
	if (core.SegmentUnwrap{Carrier: "png_text"}).Validate() != nil {
		t.Fatal("empty prefix (every text chunk) refused")
	}
	if (core.SegmentUnwrap{Carrier: "body", Magic: "M", Codec: "base64"}).Validate() != nil {
		t.Fatal("base64 codec refused")
	}
}
