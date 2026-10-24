package core

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// ExtSubtitle is an external subtitle track a plugin attaches to a resolved
// HLS stream (resolve_stream's `subtitles`). The proxy adds it to the master
// playlist as an EXT-X-MEDIA TYPE=SUBTITLES rendition, so every player (and
// the offline downloads) sees it like a native track.
type ExtSubtitle struct {
	URL     string `json:"u"`
	Lang    string `json:"l,omitempty"` // ISO 639-1/2 ("it", "ita")
	Name    string `json:"n,omitempty"`
	Default bool   `json:"d,omitempty"`
}

// maxExtSubtitles bounds the tracks one stream carries (they ride in the URL).
const maxExtSubtitles = 4

// EncodeExtSubs packs tracks for the `xsub` proxy query param ("" for none).
// Tracks without an http(s) or inline URL are dropped.
func EncodeExtSubs(subs []ExtSubtitle) string {
	clean := make([]ExtSubtitle, 0, len(subs))
	for _, s := range subs {
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") &&
			!strings.HasPrefix(s.URL, InlineSubtitleScheme) {
			continue
		}
		clean = append(clean, s)
		if len(clean) == maxExtSubtitles {
			break
		}
	}
	if len(clean) == 0 {
		return ""
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeExtSubs is EncodeExtSubs' inverse; nil on any malformed input.
func DecodeExtSubs(raw string) []ExtSubtitle {
	if raw == "" {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}
	var subs []ExtSubtitle
	if json.Unmarshal(b, &subs) != nil {
		return nil
	}
	if len(subs) > maxExtSubtitles {
		subs = subs[:maxExtSubtitles]
	}
	return subs
}

// ─── Inline subtitles ────────────────────────────────────────────────────────
//
// A plugin may hand the subtitle file itself (resolve_stream's
// subtitles[].data_base64) instead of a link: one it keeps on disk, so a
// later Play needs no new download from the subtitle site. The bytes wait
// here, under an internal URL the subtitle proxy reads instead of fetching.
// Memory only and bounded: after a restart (or an eviction) the track is
// empty until the next resolve hands the file again.

// InlineSubtitleScheme prefixes the URL of an inline subtitle.
const InlineSubtitleScheme = "mycelium-inline:"

const (
	// MaxInlineSubtitleBytes caps one inline file (zip or gzip included).
	MaxInlineSubtitleBytes = 4 << 20
	inlineSubsMaxBytes     = 32 << 20
	inlineSubsTTL          = 24 * time.Hour
)

type inlineSub struct {
	data []byte
	at   time.Time // last put or read
}

var inlineSubs = struct {
	sync.Mutex
	m     map[string]*inlineSub
	bytes int
}{m: map[string]*inlineSub{}}

// PutInlineSubtitle keeps data and returns its internal URL; false when data
// is empty or too large. The same bytes always get the same URL.
func PutInlineSubtitle(data []byte) (string, bool) {
	if len(data) == 0 || len(data) > MaxInlineSubtitleBytes {
		return "", false
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:16])
	now := time.Now()
	inlineSubs.Lock()
	defer inlineSubs.Unlock()
	if e, ok := inlineSubs.m[id]; ok {
		e.at = now
	} else {
		inlineSubs.m[id] = &inlineSub{data: data, at: now}
		inlineSubs.bytes += len(data)
	}
	// Expired first, then the least recently used, until under the ceiling.
	for k, e := range inlineSubs.m {
		if now.Sub(e.at) > inlineSubsTTL {
			inlineSubs.bytes -= len(e.data)
			delete(inlineSubs.m, k)
		}
	}
	for inlineSubs.bytes > inlineSubsMaxBytes {
		oldK := ""
		var oldT time.Time
		for k, e := range inlineSubs.m {
			if k != id && (oldK == "" || e.at.Before(oldT)) {
				oldK, oldT = k, e.at
			}
		}
		if oldK == "" {
			break
		}
		inlineSubs.bytes -= len(inlineSubs.m[oldK].data)
		delete(inlineSubs.m, oldK)
	}
	return InlineSubtitleScheme + id, true
}

// InlineSubtitle returns the bytes behind an inline subtitle URL.
func InlineSubtitle(u string) ([]byte, bool) {
	id, ok := strings.CutPrefix(u, InlineSubtitleScheme)
	if !ok {
		return nil, false
	}
	inlineSubs.Lock()
	defer inlineSubs.Unlock()
	e, ok := inlineSubs.m[id]
	if !ok {
		return nil, false
	}
	e.at = time.Now()
	return e.data, true
}
