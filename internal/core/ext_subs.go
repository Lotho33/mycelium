package core

import (
	"encoding/base64"
	"encoding/json"
	"strings"
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
// Tracks without an http(s) URL are dropped.
func EncodeExtSubs(subs []ExtSubtitle) string {
	clean := make([]ExtSubtitle, 0, len(subs))
	for _, s := range subs {
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
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
