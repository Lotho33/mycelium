package api

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
)

// Segment normalisation: what the proxy does to a fully buffered media
// segment before handing it to the player. Strict demuxers (hls.js, AVPlayer)
// want the media at offset 0 and nothing but media, while some CDNs serve
// segments in another container: an image (PNG/JPEG/GIF/BMP/WebP) in front,
// padding after the last TS packet, a mislabelled Content-Type, or an error
// page served with 200.

// segVerdict is normalizeSegment's outcome.
type segVerdict struct {
	body []byte
	ct   string
	// reject, when set, is why the body must not reach the player (an error
	// or challenge page, or no media at all under a non-media type).
	reject string
	// notes describe what was changed, for the log ("" = untouched).
	notes []string
}

// segmentTypeFromURL guesses the media type of a segment from its URL.
func segmentTypeFromURL(rawURL string) string {
	lu := strings.ToLower(rawURL)
	for _, ext := range []string{".mp4", ".m4s", ".cmfv", ".cmfa"} {
		if strings.Contains(lu, ext) {
			return "video/mp4"
		}
	}
	return "video/MP2T"
}

// mediaTypeAtStart reports the container starting at b[0] ("" = none). It
// is the cheap check: no scanning.
func mediaTypeAtStart(b []byte) string {
	if tsSyncOffset(b[:min(len(b), 4*188)]) == 0 {
		return "video/MP2T"
	}
	if fmp4BoxOffset(b[:min(len(b), 8)]) == 0 {
		return "video/mp4"
	}
	return ""
}

// normalizeSegment inspects a complete segment body. ct is the upstream
// Content-Type, encrypted whether the playlist declared an active key (the
// body is then AES-128 ciphertext, so only its wrapper can be recognised).
func normalizeSegment(body []byte, ct, rawURL string, encrypted bool) segVerdict {
	v := segVerdict{body: body, ct: ct}
	lct := strings.ToLower(strings.TrimSpace(ct))

	if encrypted {
		if looksLikeTextPage(body) {
			v.reject = "encrypted segment but body is a text page"
			return v
		}
		// Ciphertext can't be sniffed, but a wrapper image can be parsed to its
		// end; AES-CBC output is a whole number of 16-byte blocks, which
		// confirms the cut.
		if n := imageWrapperLen(body); n > 0 && len(body) > n && (len(body)-n)%16 == 0 {
			v.body = body[n:]
			v.notes = append(v.notes, "stripped "+strconv.Itoa(n)+"-byte image wrapper from ciphertext")
		}
		v.ct = segmentTypeFromURL(rawURL)
		return v
	}

	// Already media at offset 0: the common case, no scanning.
	if mt := mediaTypeAtStart(body); mt != "" {
		if !strings.HasPrefix(lct, "video/") {
			if lct != "" {
				v.notes = append(v.notes, "mislabelled "+ct+", relaying as "+mt)
			}
			v.ct = mt
		}
		v.trimTSTail()
		return v
	}

	// Audio renditions and subtitles are neither TS nor fMP4.
	if other := nonVideoSegmentType(body); other != "" {
		if !strings.HasPrefix(lct, "audio/") && !strings.HasPrefix(lct, "text/vtt") {
			v.ct = other
		}
		return v
	}

	// A wrapper parsed to its end, with media right after it.
	if n := imageWrapperLen(body); n > 0 {
		if mt := mediaTypeAtStart(body[n:]); mt != "" {
			v.body, v.ct = body[n:], mt
			v.notes = append(v.notes, "stripped "+strconv.Itoa(n)+"-byte image wrapper")
			v.trimTSTail()
			return v
		}
	}

	if looksLikeTextPage(body) {
		v.reject = "body is a text page, not media"
		return v
	}

	// Any other prefix: look for where the media starts.
	scan := body[:min(len(body), mediaSniffLimit)]
	if off, mt := mediaStartOffset(scan); off >= 0 {
		v.body, v.ct = body[off:], mt
		v.notes = append(v.notes, "media at offset "+strconv.Itoa(off)+", stripped prefix")
		v.trimTSTail()
		return v
	}

	// No signature. Media-ish types pass raw (an unknown container is not
	// ours to judge); anything else is an error page.
	switch {
	case lct == "", strings.HasPrefix(lct, "image/"), strings.HasPrefix(lct, "binary/"):
		v.ct = "video/MP2T"
		v.notes = append(v.notes, "no media signature, passing raw bytes")
	case strings.HasPrefix(lct, "video/"), strings.HasPrefix(lct, "audio/"),
		strings.HasPrefix(lct, "application/octet-stream"):
	default:
		v.reject = "no media signature under content-type " + ct
	}
	return v
}

// trimTSTail drops trailing bytes after the last whole MPEG-TS packet when
// they are junk (an appended image, padding): only when no TS sync pattern
// resumes later, so a segment with a corrupt packet mid-way keeps the rest.
func (v *segVerdict) trimTSTail() {
	if v.ct != "video/MP2T" && !strings.EqualFold(v.ct, "video/mp2t") {
		return
	}
	b := v.body
	end := 0
	for end+188 <= len(b) && b[end] == 0x47 {
		end += 188
	}
	if end == 0 || end == len(b) {
		return
	}
	if tsSyncOffset(b[end:]) >= 0 {
		return
	}
	v.notes = append(v.notes, "trimmed "+strconv.Itoa(len(b)-end)+" trailing bytes after the last TS packet")
	v.body = b[:end]
}

// imageWrapperLen returns the length of a complete image file at the start
// of b (PNG, JPEG, GIF, BMP, WebP), parsed structurally, or 0 when b doesn't
// start with one or it is truncated.
func imageWrapperLen(b []byte) int {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return pngLen(b)
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return jpegLen(b)
	case bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a")):
		return gifLen(b)
	case len(b) >= 14 && b[0] == 'B' && b[1] == 'M':
		n := int(binary.LittleEndian.Uint32(b[2:6]))
		if n >= 14 && n < len(b) {
			return n
		}
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		n := 8 + int(binary.LittleEndian.Uint32(b[4:8]))
		n += n & 1 // RIFF pads odd chunks
		if n > 12 && n < len(b) {
			return n
		}
	}
	return 0
}

// pngLen walks the chunks to the end of IEND.
func pngLen(b []byte) int {
	p := 8
	for p+12 <= len(b) {
		n := int(binary.BigEndian.Uint32(b[p : p+4]))
		if n < 0 || n > len(b) {
			return 0
		}
		typ := string(b[p+4 : p+8])
		p += 12 + n
		if p > len(b) {
			return 0
		}
		if typ == "IEND" {
			return p
		}
	}
	return 0
}

// jpegLen skips the marker segments by their lengths up to the scan data,
// then finds EOI (FF D9) in the entropy-coded data (where a literal FF is
// always followed by 00 or a restart marker).
func jpegLen(b []byte) int {
	p := 2
	for p+2 <= len(b) {
		if b[p] != 0xFF {
			return 0
		}
		m := b[p+1]
		switch {
		case m == 0xFF: // fill byte
			p++
			continue
		case m == 0xD9:
			return p + 2
		case m == 0x01 || (m >= 0xD0 && m <= 0xD7): // no length
			p += 2
			continue
		}
		if p+4 > len(b) {
			return 0
		}
		n := int(binary.BigEndian.Uint16(b[p+2 : p+4]))
		if n < 2 {
			return 0
		}
		p += 2 + n
		if m != 0xDA {
			continue
		}
		// Entropy-coded data until the next real marker.
		for p+1 < len(b) {
			if b[p] == 0xFF && b[p+1] != 0x00 && (b[p+1] < 0xD0 || b[p+1] > 0xD7) {
				break
			}
			p++
		}
	}
	return 0
}

// gifLen walks the GIF blocks up to the trailer (0x3B).
func gifLen(b []byte) int {
	if len(b) < 13 {
		return 0
	}
	p := 13
	if flags := b[10]; flags&0x80 != 0 {
		p += 3 << ((flags & 0x07) + 1)
	}
	skipSubBlocks := func(p int) int {
		for p < len(b) {
			n := int(b[p])
			p++
			if n == 0 {
				return p
			}
			p += n
		}
		return -1
	}
	for p < len(b) {
		switch b[p] {
		case 0x3B:
			return p + 1
		case 0x21: // extension: introducer, label, sub-blocks
			if p = skipSubBlocks(p + 2); p < 0 {
				return 0
			}
		case 0x2C: // image descriptor, optional local table, LZW size, data
			if p+10 > len(b) {
				return 0
			}
			flags := b[p+9]
			p += 10
			if flags&0x80 != 0 {
				p += 3 << ((flags & 0x07) + 1)
			}
			if p = skipSubBlocks(p + 1); p < 0 {
				return 0
			}
		default:
			return 0
		}
	}
	return 0
}
