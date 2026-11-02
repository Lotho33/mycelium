package api

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"io"

	"mycelium/internal/core"
)

// Segments packed inside another container by the source, unpacked with the
// rules its plugin declares (manifest `segments.unwrap`, core.SegmentUnwrap).
// This file only knows generic carriers — a response body, the samples of a
// PNG, a WebP chunk — and codecs; the bytes to look for come from the plugin.
// Every size the source declares is bounded before anything is allocated.

// unwrapMaxPixels bounds the canvas a PNG may declare before any of it is
// inflated: a tiny file can declare gigabytes. 32 Mpx of RGBA is 128 MiB.
const unwrapMaxPixels = 32 << 20

// unwrapMaxWidth bounds one row: three row buffers are allocated before
// anything is checked, so a 1-row canvas within unwrapMaxPixels would still
// cost hundreds of MiB.
const unwrapMaxWidth = 16384

// applySegmentUnwraps tries rules in order and returns the first media it
// extracts (nil when none applies); how describes the rule, for the log.
func applySegmentUnwraps(b []byte, rules []core.SegmentUnwrap) (media []byte, how string) {
	for _, r := range rules {
		if m := unwrapOne(b, r); m != nil && mediaTypeAtStart(m) != "" {
			return m, "plugin rule " + r.Carrier
		}
	}
	return nil, ""
}

func unwrapOne(b []byte, r core.SegmentUnwrap) []byte {
	magic := []byte(r.Magic)
	switch r.Carrier {
	case core.UnwrapCarrierBody:
		i := bytes.Index(b, magic)
		if i < 0 || len(magic) == 0 {
			return nil
		}
		return framedPayload(b[i+len(magic):], r)
	case core.UnwrapCarrierWebPChunk:
		d := webpChunkData(b, r.Chunk)
		if d == nil || !bytes.HasPrefix(d, magic) {
			return nil
		}
		return framedPayload(d[len(magic):], r)
	case core.UnwrapCarrierPNGPixels:
		return pngPixelsPayload(b, r)
	}
	return nil
}

func lengthFieldSize(r core.SegmentUnwrap) int {
	if r.Length == "" {
		return 0
	}
	return 4
}

// framedPayload reads the optional length field at the start of p, then
// decodes the payload with the rule's codec.
func framedPayload(p []byte, r core.SegmentUnwrap) []byte {
	if r.Length != "" {
		if len(p) < 4 {
			return nil
		}
		var n uint32
		if r.Length == "u32le" {
			n = binary.LittleEndian.Uint32(p[:4])
		} else {
			n = binary.BigEndian.Uint32(p[:4])
		}
		p = p[4:]
		if n == 0 || uint64(n) > uint64(len(p)) {
			return nil
		}
		p = p[:n]
	}
	switch r.Codec {
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(p))
		if err != nil {
			return nil
		}
		defer zr.Close()
		return readAllCapped(zr)
	case "zlib":
		zr, err := zlib.NewReader(bytes.NewReader(p))
		if err != nil {
			return nil
		}
		defer zr.Close()
		return readAllCapped(zr)
	}
	return p
}

// readAllCapped inflates up to segmentBufferLimit; nil on error or beyond.
func readAllCapped(r io.Reader) []byte {
	out, err := io.ReadAll(io.LimitReader(r, segmentBufferLimit+1))
	if err != nil || len(out) > segmentBufferLimit {
		return nil
	}
	return out
}

// webpChunkData returns the data of the first chunk named fourcc in a WebP
// file, nil when absent or b isn't one.
func webpChunkData(b []byte, fourcc string) []byte {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return nil
	}
	p := 12
	for p+8 <= len(b) {
		tag := string(b[p : p+4])
		n := int(binary.LittleEndian.Uint32(b[p+4 : p+8]))
		p += 8
		if n < 0 || n > len(b)-p {
			return nil
		}
		if tag == fourcc {
			return b[p : p+n]
		}
		p += n + n&1
	}
	return nil
}

// pngPixelsPayload decodes the samples of an 8-bit, non-interlaced RGB or
// RGBA PNG (alpha dropped) only as far as the rule needs: the magic is
// checked as soon as the first samples are in, so an ordinary image costs a
// row or two, and with a length field decoding stops at the payload's end.
func pngPixelsPayload(b []byte, r core.SegmentUnwrap) []byte {
	if !bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) {
		return nil
	}
	var w, h, ctype int
	var idat [][]byte
	p := 8
	for p+12 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[p : p+4]))
		if n < 0 || n > len(b)-p-12 {
			return nil
		}
		typ := string(b[p+4 : p+8])
		data := b[p+8 : p+8+n]
		switch typ {
		case "IHDR":
			if n < 13 {
				return nil
			}
			w = int(binary.BigEndian.Uint32(data[0:4]))
			h = int(binary.BigEndian.Uint32(data[4:8]))
			depth, interlace := data[8], data[12]
			ctype = int(data[9])
			if depth != 8 || interlace != 0 || (ctype != 2 && ctype != 6) {
				return nil
			}
		case "IDAT":
			idat = append(idat, data)
		}
		p += 12 + n
		if typ == "IEND" {
			break
		}
	}
	// Width first, then h against the quotient: w*h of two uint32 values can
	// overflow int64 and pass a plain product check.
	if w <= 0 || h <= 0 || w > unwrapMaxWidth || h > unwrapMaxPixels/w || len(idat) == 0 {
		return nil
	}
	bpp := 3
	if ctype == 6 {
		bpp = 4
	}
	stride := w * bpp
	total := w * h * 3 // samples in the image, alpha dropped

	magic := []byte(r.Magic)
	head := len(magic) + lengthFieldSize(r)
	// Without a length field the payload runs to the last sample: bounded by
	// what a segment may be.
	need := total
	if limit := head + segmentBufferLimit; need > limit {
		need = limit
	}
	headChecked := false

	readers := make([]io.Reader, len(idat))
	for i, d := range idat {
		readers[i] = bytes.NewReader(d)
	}
	zr, err := zlib.NewReader(io.MultiReader(readers...))
	if err != nil {
		return nil
	}
	defer zr.Close()

	row := make([]byte, 1+stride)
	prev := make([]byte, stride)
	cur := make([]byte, stride)
	samples := make([]byte, 0, min(need, 1<<20))
	for y := 0; y < h && len(samples) < need; y++ {
		if _, err := io.ReadFull(zr, row); err != nil {
			return nil
		}
		if !pngUnfilter(row[0], row[1:], cur, prev, bpp) {
			return nil
		}
		for i := 0; i < stride; i += bpp {
			samples = append(samples, cur[i], cur[i+1], cur[i+2])
		}
		prev, cur = cur, prev
		if !headChecked && len(samples) >= head {
			headChecked = true
			if !bytes.HasPrefix(samples, magic) {
				return nil
			}
			if r.Length != "" {
				f := samples[len(magic):head]
				var n uint32
				if r.Length == "u32le" {
					n = binary.LittleEndian.Uint32(f)
				} else {
					n = binary.BigEndian.Uint32(f)
				}
				if n == 0 || uint64(n) > uint64(segmentBufferLimit) || head+int(n) > total {
					return nil
				}
				need = head + int(n)
			}
		}
	}
	if !headChecked || len(samples) < need && r.Length != "" {
		return nil
	}
	if len(samples) > need {
		samples = samples[:need]
	}
	return framedPayload(samples[len(magic):], r)
}

// pngUnfilter reverses one row's PNG filter into cur (prev is the previous
// reconstructed row, zeros for the first). false on an unknown filter.
func pngUnfilter(filter byte, src, cur, prev []byte, bpp int) bool {
	switch filter {
	case 0:
		copy(cur, src)
	case 1:
		for i := range src {
			var a byte
			if i >= bpp {
				a = cur[i-bpp]
			}
			cur[i] = src[i] + a
		}
	case 2:
		for i := range src {
			cur[i] = src[i] + prev[i]
		}
	case 3:
		for i := range src {
			var a int
			if i >= bpp {
				a = int(cur[i-bpp])
			}
			cur[i] = src[i] + byte((a+int(prev[i]))/2)
		}
	case 4:
		for i := range src {
			var a, c int
			if i >= bpp {
				a, c = int(cur[i-bpp]), int(prev[i-bpp])
			}
			cur[i] = src[i] + paeth(a, int(prev[i]), c)
		}
	default:
		return false
	}
	return true
}

func paeth(a, b, c int) byte {
	p := a + b - c
	pa, pb, pc := abs(p-a), abs(p-b), abs(p-c)
	switch {
	case pa <= pb && pa <= pc:
		return byte(a)
	case pb <= pc:
		return byte(b)
	}
	return byte(c)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
