package core

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// PlaylistSeal is a rule a plugin declares (manifest `playlist.sealed_uris`)
// when its source encrypts the segment URIs of its HLS media playlists and
// decrypts them in the page's JavaScript. The proxy knows the ciphers below;
// the key is per stream: the plugin's resolve appends it to the playlist URL
// as the fragment "#mpk=<base64url key>". A fragment is never sent upstream,
// and it rides along every re-resolve, variant and recovery of the URL, so a
// fresh key arrives with every fresh token.
//
//	cipher   aes-128-ctr (key: 16 bytes) | aes-256-ctr (32 bytes)
//	counter  media_sequence: the 16-byte initial counter block is zero but
//	         for the segment's media sequence number, big-endian, in its
//	         last 8 bytes (the first segment line has #EXT-X-MEDIA-SEQUENCE,
//	         each following one the next number)
//
// Each segment line is base64url ciphertext; what it decrypts to must be an
// http(s) URL or a path, or the line is left as it was (a playlist already in
// clear passes through untouched).
type PlaylistSeal struct {
	Cipher  string `yaml:"cipher" json:"cipher"`
	Counter string `yaml:"counter" json:"counter"`
}

const (
	SealCipherAES128CTR = "aes-128-ctr"
	SealCipherAES256CTR = "aes-256-ctr"
	SealCounterMediaSeq = "media_sequence"
	playlistSealKeyFrag = "mpk="
	maxSealedLineBytes  = 8 << 10
)

// Validate reports why the rule can't be used, nil when it can.
func (s PlaylistSeal) Validate() error {
	switch s.Cipher {
	case SealCipherAES128CTR, SealCipherAES256CTR:
	default:
		return fmt.Errorf("cipher %q sconosciuto (aes-128-ctr | aes-256-ctr)", s.Cipher)
	}
	if s.Counter != SealCounterMediaSeq {
		return fmt.Errorf("counter %q sconosciuto (media_sequence)", s.Counter)
	}
	return nil
}

func (s PlaylistSeal) keyLen() int {
	if s.Cipher == SealCipherAES256CTR {
		return 32
	}
	return 16
}

// PlaylistSealKey returns the key carried by u's "#mpk=" fragment (nil when
// there is none or it doesn't decode).
func PlaylistSealKey(u string) []byte {
	i := strings.LastIndexByte(u, '#')
	if i < 0 {
		return nil
	}
	for _, part := range strings.Split(u[i+1:], "&") {
		if v, ok := strings.CutPrefix(part, playlistSealKeyFrag); ok {
			if k, err := b64urlLoose(v); err == nil {
				return k
			}
		}
	}
	return nil
}

// PlaylistSealFragment returns u's "#…" fragment when it carries a key (to
// pass it on to the URLs derived from u), else "".
func PlaylistSealFragment(u string) string {
	i := strings.LastIndexByte(u, '#')
	if i < 0 || !strings.Contains(u[i:], playlistSealKeyFrag) {
		return ""
	}
	return u[i:]
}

func b64urlLoose(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// UnsealPlaylist decrypts the segment lines of a media playlist sealed by
// rule with key. It returns content unchanged (and false) when there is
// nothing to do: no key, a master playlist, or no line that decrypts to a URL.
func UnsealPlaylist(content []byte, key []byte, rule PlaylistSeal) ([]byte, bool, error) {
	if len(key) == 0 || rule.Validate() != nil {
		return content, false, nil
	}
	if len(key) != rule.keyLen() {
		return content, false, fmt.Errorf("chiave playlist di %d byte, %s ne vuole %d", len(key), rule.Cipher, rule.keyLen())
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return content, false, err
	}
	lines := strings.Split(string(content), "\n")
	var seq uint64
	changed := false
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if v, ok := strings.CutPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"); ok {
				seq, _ = strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			}
			if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
				return content, false, nil // a master: its URIs are never sealed
			}
			continue
		}
		cur := seq
		seq++
		if plain, ok := unsealLine(block, line, cur); ok {
			lines[i] = plain
			changed = true
		}
	}
	if !changed {
		return content, false, nil
	}
	return []byte(strings.Join(lines, "\n")), true, nil
}

func unsealLine(block cipher.Block, line string, seq uint64) (string, bool) {
	if len(line) > maxSealedLineBytes || strings.Contains(line, "://") {
		return "", false
	}
	ct, err := b64urlLoose(line)
	if err != nil || len(ct) == 0 {
		return "", false
	}
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], seq)
	pt := make([]byte, len(ct))
	cipher.NewCTR(block, iv).XORKeyStream(pt, ct)
	s := string(pt)
	if !plausibleSegmentURI(s) {
		return "", false
	}
	return s, true
}

// plausibleSegmentURI: an absolute http(s) URL or a path, printable ASCII.
func plausibleSegmentURI(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	if strings.HasPrefix(s, "/") {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
