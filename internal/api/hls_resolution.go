package api

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/managers"
)

// Real dimensions of HLS variants. Some masters declare a RESOLUTION that
// isn't the stream's (a "1920x1080" variant that decodes as 1920x1440), and
// a player picks its variant from that attribute: a decoder limited to 1080p
// then fails on its first segment instead of the player skipping the variant
// up front. The pre-buffer already holds the top variant's first segment, so
// it reads the dimensions from the H.264 SPS there (decrypting AES-128 with
// the playlist's key when needed) and ProxyPlaylist corrects RESOLUTION when
// it rewrites the master.

type variantDimsEntry struct {
	w, h int
	at   time.Time
}

var variantDimsCache sync.Map // variant playlist URL → variantDimsEntry

// variantDimsTTL bounds how long measured dimensions are trusted: the URLs
// carry short-lived tokens, so entries matter for one playback session.
const variantDimsTTL = 2 * time.Hour

func rememberVariantDims(variantURL string, w, h int) {
	variantDimsCache.Store(variantURL, variantDimsEntry{w: w, h: h, at: time.Now()})
	// Drop expired entries now and then (the map is keyed by tokenised URLs).
	if time.Now().Unix()%16 == 0 {
		variantDimsCache.Range(func(k, v any) bool {
			if time.Since(v.(variantDimsEntry).at) > variantDimsTTL {
				variantDimsCache.Delete(k)
			}
			return true
		})
	}
}

func lookupVariantDims(variantURL string) (w, h int, ok bool) {
	v, found := variantDimsCache.Load(variantURL)
	if !found {
		return 0, 0, false
	}
	e := v.(variantDimsEntry)
	if time.Since(e.at) > variantDimsTTL {
		return 0, 0, false
	}
	return e.w, e.h, true
}

var reResolution = regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`)

// correctStreamInfResolution sets RESOLUTION on an #EXT-X-STREAM-INF line to
// the measured w×h, reporting what was declared ("" when absent) and whether
// the line changed.
func correctStreamInfResolution(line string, w, h int) (string, string, bool) {
	want := fmt.Sprintf("RESOLUTION=%dx%d", w, h)
	if m := reResolution.FindStringSubmatchIndex(line); m != nil {
		declared := line[m[2]:m[3]] + "x" + line[m[4]:m[5]]
		if line[m[0]:m[1]] == want {
			return line, declared, false
		}
		return line[:m[0]] + want + line[m[1]:], declared, true
	}
	return line + "," + want, "", true
}

// ── probing ─────────────────────────────────────────────────────────────────

// probeVariantDims reads the video dimensions from a variant's first media
// bytes: body is segURL as fetched (already normalised), mediaBody the
// variant's playlist. Encrypted bodies are decrypted with the playlist's
// key, fetched through client.
func probeVariantDims(ctx context.Context, client *http.Client, opts managers.PrefetchOptions, mediaURL string, mediaBody []byte, segURL string, body []byte) (w, h int, ok bool) {
	key := keyForSegment(mediaURL, mediaBody, segURL)
	if key.uri == "" {
		return videoDims(body)
	}
	k, err := fetchHLSKey(ctx, client, opts, key.uri)
	if err != nil {
		return 0, 0, false
	}
	plain, err := aesCBCDecryptPrefix(body, k, key.iv)
	if err != nil {
		return 0, 0, false
	}
	return videoDims(plain)
}

// segKey is the AES-128 key in effect for one segment (uri "" = clear).
type segKey struct {
	uri string
	iv  []byte
}

// keyForSegment finds which #EXT-X-KEY applies to segURL (a segment or an
// #EXT-X-MAP init section) and its IV: the IV attribute, or else the
// segment's media sequence number (for an init section, the next segment's).
func keyForSegment(mediaURL string, mediaBody []byte, segURL string) segKey {
	base := mediaURL
	if i := strings.LastIndex(mediaURL, "/"); i >= 0 {
		base = mediaURL[:i+1]
	}
	var cur segKey
	seq := uint64(0)
	pendingMap := false
	var mapKey segKey
	for _, raw := range strings.Split(string(mediaBody), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			seq, _ = strconv.ParseUint(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(line, "#EXT-X-KEY"):
			cur = segKey{}
			if keyLineEncrypts(line) && strings.Contains(strings.ToUpper(line), "METHOD=AES-128") {
				if m := reKeyURI.FindStringSubmatch(line); len(m) == 2 {
					cur.uri = proxyURLJoin(base, m[1])
				}
				if i := strings.Index(strings.ToUpper(line), "IV=0X"); i >= 0 {
					hexIV := line[i+5:]
					if j := strings.IndexAny(hexIV, ", "); j >= 0 {
						hexIV = hexIV[:j]
					}
					if len(hexIV) < 32 {
						hexIV = strings.Repeat("0", 32-len(hexIV)) + hexIV
					}
					if b, err := hex.DecodeString(hexIV); err == nil && len(b) == 16 {
						cur.iv = b
					}
				}
			}
		case strings.HasPrefix(line, "#EXT-X-MAP"):
			if m := reKeyURI.FindStringSubmatch(line); len(m) == 2 && proxyURLJoin(base, m[1]) == segURL {
				pendingMap, mapKey = true, cur
			}
		case strings.HasPrefix(line, "#"):
		default:
			k := cur
			if pendingMap {
				k = mapKey
			}
			if k.uri != "" && k.iv == nil {
				k.iv = make([]byte, 16)
				binary.BigEndian.PutUint64(k.iv[8:], seq)
			}
			if pendingMap || proxyURLJoin(base, line) == segURL {
				return k
			}
			seq++
		}
	}
	return segKey{}
}

// fetchHLSKey GETs a 16-byte AES-128 key.
func fetchHLSKey(ctx context.Context, client *http.Client, opts managers.PrefetchOptions, keyURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := prefetchReq(ctx, http.MethodGet, keyURL, opts)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key: upstream %d", resp.StatusCode)
	}
	k, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil || len(k) != 16 {
		return nil, fmt.Errorf("key: %d bytes", len(k))
	}
	return k, nil
}

// aesCBCDecryptPrefix decrypts the whole 16-byte blocks of data (CBC needs
// no more than the previous block, so a prefix decrypts on its own).
func aesCBCDecryptPrefix(data, key, iv []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	n := len(data) / aes.BlockSize * aes.BlockSize
	out := make([]byte, n)
	cipher.NewCBCDecrypter(blk, iv).CryptBlocks(out, data[:n])
	return out, nil
}

// videoDims finds an H.264 SPS in an fMP4 init section (avcC) or in MPEG-TS
// and returns the coded picture size after cropping.
func videoDims(b []byte) (w, h int, ok bool) {
	if sps := spsFromAvcC(b); sps != nil {
		if w, h, ok := h264SPSDims(sps); ok {
			return w, h, true
		}
	}
	if sps := spsFromTS(b); sps != nil {
		return h264SPSDims(sps)
	}
	return 0, 0, false
}

// spsFromAvcC returns the first SPS of an avcC box (AVCDecoderConfigurationRecord).
func spsFromAvcC(b []byte) []byte {
	i := bytes.Index(b, []byte("avcC"))
	if i < 0 || i+4+8 > len(b) {
		return nil
	}
	rec := b[i+4:]
	if rec[0] != 1 || rec[5]&0x1F == 0 {
		return nil
	}
	n := int(binary.BigEndian.Uint16(rec[6:8]))
	if n == 0 || 8+n > len(rec) {
		return nil
	}
	return rec[8 : 8+n]
}

// spsFromTS concatenates each PID's packet payloads and looks for an Annex B
// SPS NAL (type 7) in them.
func spsFromTS(b []byte) []byte {
	off := tsSyncOffset(b[:min(len(b), 4*188)])
	if off < 0 {
		return nil
	}
	streams := map[uint16][]byte{}
	for p := off; p+188 <= len(b); p += 188 {
		pkt := b[p : p+188]
		if pkt[0] != 0x47 {
			break
		}
		pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
		if pid == 0 || pid == 0x1FFF {
			continue
		}
		start := 4
		switch (pkt[3] >> 4) & 0x3 {
		case 0, 2: // no payload
			continue
		case 3: // adaptation field first
			start += 1 + int(pkt[4])
		}
		if start >= 188 {
			continue
		}
		streams[pid] = append(streams[pid], pkt[start:]...)
	}
	for _, data := range streams {
		if sps := findAnnexBNAL(data, 7); sps != nil {
			return sps
		}
	}
	return nil
}

// findAnnexBNAL returns the first NAL unit of the given H.264 type in an
// Annex B byte stream (start codes 00 00 01), without its start code.
func findAnnexBNAL(data []byte, nalType byte) []byte {
	for i := 0; i+3 < len(data); i++ {
		if data[i] != 0 || data[i+1] != 0 || data[i+2] != 1 {
			continue
		}
		if data[i+3]&0x1F != nalType {
			continue
		}
		rest := data[i+3:]
		if end := bytes.Index(rest[1:], []byte{0, 0, 1}); end >= 0 {
			return rest[:end+1]
		}
		return rest
	}
	return nil
}

// h264SPSDims parses an SPS NAL (with its header byte) up to the frame
// cropping and returns the displayed width and height.
func h264SPSDims(nal []byte) (w, h int, ok bool) {
	if len(nal) < 4 || nal[0]&0x1F != 7 {
		return 0, 0, false
	}
	r := &bitReader{b: unescapeRBSP(nal[1:])}
	profile := r.bits(8)
	r.bits(16) // constraint flags + level
	r.ue()     // seq_parameter_set_id
	chroma := uint(1)
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = r.ue()
		if chroma == 3 {
			r.bits(1) // separate_colour_plane_flag
		}
		r.ue()              // bit_depth_luma_minus8
		r.ue()              // bit_depth_chroma_minus8
		r.bits(1)           // qpprime_y_zero_transform_bypass_flag
		if r.bits(1) == 1 { // seq_scaling_matrix_present_flag
			n := 8
			if chroma == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.bits(1) == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := 8, 8
					for j := 0; j < size; j++ {
						if next != 0 {
							next = (last + r.se() + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}
	r.ue()          // log2_max_frame_num_minus4
	switch r.ue() { // pic_order_cnt_type
	case 0:
		r.ue()
	case 1:
		r.bits(1)
		r.se()
		r.se()
		for n := r.ue(); n > 0 && !r.err; n-- {
			r.se()
		}
	}
	r.ue()    // max_num_ref_frames
	r.bits(1) // gaps_in_frame_num_value_allowed_flag
	wMbs := int(r.ue()) + 1
	hMapUnits := int(r.ue()) + 1
	frameMbsOnly := int(r.bits(1))
	if frameMbsOnly == 0 {
		r.bits(1) // mb_adaptive_frame_field_flag
	}
	r.bits(1) // direct_8x8_inference_flag
	var cl, cr, ct, cb int
	if r.bits(1) == 1 {
		cl, cr, ct, cb = int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
	}
	if r.err {
		return 0, 0, false
	}
	cropX, cropY := 1, 2-frameMbsOnly
	switch chroma {
	case 1:
		cropX, cropY = 2, 2*(2-frameMbsOnly)
	case 2:
		cropX = 2
	}
	w = wMbs*16 - cropX*(cl+cr)
	h = (2-frameMbsOnly)*hMapUnits*16 - cropY*(ct+cb)
	if w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return 0, 0, false
	}
	return w, h, true
}

// unescapeRBSP removes emulation-prevention bytes (00 00 03 → 00 00).
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// bitReader reads big-endian bits and Exp-Golomb codes; err is set on
// running out of data (reads then return 0).
type bitReader struct {
	b   []byte
	pos int
	err bool
}

func (r *bitReader) bits(n int) uint {
	var v uint
	for i := 0; i < n; i++ {
		if r.pos >= len(r.b)*8 {
			r.err = true
			return 0
		}
		bit := (r.b[r.pos/8] >> (7 - uint(r.pos%8))) & 1
		v = v<<1 | uint(bit)
		r.pos++
	}
	return v
}

func (r *bitReader) ue() uint {
	zeros := 0
	for r.bits(1) == 0 {
		if r.err || zeros > 31 {
			r.err = true
			return 0
		}
		zeros++
	}
	return (1<<uint(zeros) - 1) + r.bits(zeros)
}

func (r *bitReader) se() int {
	v := r.ue()
	if v%2 == 1 {
		return int(v+1) / 2
	}
	return -int(v / 2)
}
