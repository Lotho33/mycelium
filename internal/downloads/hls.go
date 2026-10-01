package downloads

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ─── HLS playlist parsing ────────────────────────────────────────────────────
// Just enough of RFC 8216 to offer download choices and pick the chosen
// renditions again later: variants (EXT-X-STREAM-INF), audio and subtitle
// renditions (EXT-X-MEDIA) and a media playlist's total duration. URIs are
// resolved against the playlist URL; in practice they already are absolute
// mycelium /proxy URLs (the proxy rewrites every child).

// Variant is one EXT-X-STREAM-INF entry.
type Variant struct {
	URI       string
	Width     int
	Height    int
	Bandwidth int64 // AVERAGE-BANDWIDTH when declared, else BANDWIDTH
	Peak      bool  // Bandwidth is the peak BANDWIDTH (no average declared)
	Codecs    string
	Audio     string // AUDIO group id
	Subtitles string // SUBTITLES group id
}

// Rendition is one EXT-X-MEDIA entry (TYPE=AUDIO or SUBTITLES).
type Rendition struct {
	Type     string // "AUDIO" | "SUBTITLES"
	Group    string
	Name     string
	Language string
	Default  bool
	URI      string // "" = carried inside the variant stream itself
}

// Master is a parsed multivariant playlist. A plain media playlist parses to
// one variant pointing at itself with no size information.
type Master struct {
	Variants   []Variant
	Renditions []Rendition
	IsMaster   bool
}

// parseAttrs splits an attribute list (A=1,B="x,y",C=z) respecting quotes.
func parseAttrs(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				val, s = s[1:], ""
			} else {
				val, s = s[1:1+end], s[2+end:]
			}
			s = strings.TrimPrefix(s, ",")
		} else {
			comma := strings.IndexByte(s, ',')
			if comma < 0 {
				val, s = s, ""
			} else {
				val, s = s[:comma], s[comma+1:]
			}
		}
		out[strings.ToUpper(key)] = strings.TrimSpace(val)
	}
	return out
}

func resolveURI(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// ParsePlaylist parses content fetched from playlistURL.
func ParsePlaylist(content, playlistURL string) Master {
	var m Master
	lines := strings.Split(content, "\n")
	var pending *Variant
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			m.IsMaster = true
			a := parseAttrs(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			v := Variant{Codecs: a["CODECS"], Audio: a["AUDIO"], Subtitles: a["SUBTITLES"]}
			if avg, err := strconv.ParseInt(a["AVERAGE-BANDWIDTH"], 10, 64); err == nil && avg > 0 {
				v.Bandwidth = avg
			} else if bw, err := strconv.ParseInt(a["BANDWIDTH"], 10, 64); err == nil {
				v.Bandwidth, v.Peak = bw, true
			}
			if w, h, ok := strings.Cut(a["RESOLUTION"], "x"); ok {
				v.Width, _ = strconv.Atoi(w)
				v.Height, _ = strconv.Atoi(h)
			}
			pending = &v
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			m.IsMaster = true
			a := parseAttrs(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			t := strings.ToUpper(a["TYPE"])
			if t != "AUDIO" && t != "SUBTITLES" {
				continue
			}
			r := Rendition{Type: t, Group: a["GROUP-ID"], Name: a["NAME"], Language: a["LANGUAGE"],
				Default: strings.EqualFold(a["DEFAULT"], "YES")}
			if a["URI"] != "" {
				r.URI = resolveURI(playlistURL, a["URI"])
			}
			m.Renditions = append(m.Renditions, r)
		case strings.HasPrefix(line, "#"):
			// other tags: irrelevant here
		default:
			if pending != nil {
				pending.URI = resolveURI(playlistURL, line)
				m.Variants = append(m.Variants, *pending)
				pending = nil
			}
		}
	}
	if !m.IsMaster {
		m.Variants = []Variant{{URI: playlistURL}}
	}
	// Best first: height, then bandwidth.
	sort.SliceStable(m.Variants, func(i, j int) bool {
		if m.Variants[i].Height != m.Variants[j].Height {
			return m.Variants[i].Height > m.Variants[j].Height
		}
		return m.Variants[i].Bandwidth > m.Variants[j].Bandwidth
	})
	return m
}

// MediaDuration sums a media playlist's #EXTINF durations. ended reports
// #EXT-X-ENDLIST (VOD); a media playlist without it is live/growing.
func MediaDuration(content string) (seconds float64, ended bool) {
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			if d, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && d > 0 {
				seconds += d
			}
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			ended = true
		case strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:VOD"):
			ended = true
		}
	}
	return seconds, ended
}

// assumedAudioBitrate estimates a separate audio rendition (HLS declares no
// bitrate for them): typical stereo AAC.
const assumedAudioBitrate = 128_000

func estimateBytes(bitsPerSec int64, seconds float64) int64 {
	if bitsPerSec <= 0 || seconds <= 0 {
		return 0
	}
	return int64(float64(bitsPerSec) / 8 * seconds)
}

func variantLabel(v Variant) string {
	if v.Height > 0 {
		return fmt.Sprintf("%dp", v.Height)
	}
	if v.Bandwidth > 0 {
		return fmt.Sprintf("%.1f Mbit/s", float64(v.Bandwidth)/1e6)
	}
	return "Originale"
}

// ─── selection ids ───────────────────────────────────────────────────────────
// Ids handed to the client encode *criteria*, not URLs: a scheduled download
// may run hours later against a freshly resolved master whose URLs (tokens)
// all changed, so the choice is matched again by height/bandwidth and by
// language/name.

func variantID(v Variant) string { return fmt.Sprintf("v:%d:%d", v.Height, v.Bandwidth) }

func trackID(r Rendition) string {
	kind := "a"
	if r.Type == "SUBTITLES" {
		kind = "s"
	}
	return kind + ":" + url.QueryEscape(r.Language) + ":" + url.QueryEscape(r.Name)
}

// Selection is what a download asked for, stored with the job.
type Selection struct {
	Height    int              `json:"height"`
	Bandwidth int64            `json:"bandwidth"`
	Audio     []TrackCriterion `json:"audio,omitempty"`
	Subtitles []TrackCriterion `json:"subtitles,omitempty"`
}

// TrackCriterion identifies a rendition by language and name.
type TrackCriterion struct {
	Language string `json:"language"`
	Name     string `json:"name"`
}

func parseVariantID(id string) (height int, bw int64, ok bool) {
	parts := strings.Split(id, ":")
	if len(parts) != 3 || parts[0] != "v" {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[1])
	b, err2 := strconv.ParseInt(parts[2], 10, 64)
	return h, b, err1 == nil && err2 == nil
}

func parseTrackID(id, kind string) (TrackCriterion, bool) {
	parts := strings.Split(id, ":")
	if len(parts) != 3 || parts[0] != kind {
		return TrackCriterion{}, false
	}
	lang, err1 := url.QueryUnescape(parts[1])
	name, err2 := url.QueryUnescape(parts[2])
	return TrackCriterion{Language: lang, Name: name}, err1 == nil && err2 == nil
}

// pickVariant finds the variant closest to the selection: same height with
// the nearest bandwidth, else the best one below that height, else the
// lowest one above it. Height 0 = best available.
func pickVariant(vs []Variant, sel Selection) (Variant, bool) {
	if len(vs) == 0 {
		return Variant{}, false
	}
	if sel.Height == 0 && sel.Bandwidth == 0 {
		return vs[0], true
	}
	best, found := Variant{}, false
	var bestDiff int64
	for _, v := range vs {
		if v.Height != sel.Height {
			continue
		}
		d := v.Bandwidth - sel.Bandwidth
		if d < 0 {
			d = -d
		}
		if !found || d < bestDiff {
			best, bestDiff, found = v, d, true
		}
	}
	if found {
		return best, true
	}
	for _, v := range vs { // sorted best first: first one below is the best below
		if v.Height < sel.Height {
			return v, true
		}
	}
	return vs[len(vs)-1], true
}

// pickRendition matches a criterion within renditions of one type/group:
// language+name, then language alone.
func pickRendition(rs []Rendition, c TrackCriterion) (Rendition, bool) {
	for _, r := range rs {
		if r.Language == c.Language && r.Name == c.Name {
			return r, true
		}
	}
	for _, r := range rs {
		if c.Language != "" && strings.EqualFold(r.Language, c.Language) {
			return r, true
		}
	}
	return Rendition{}, false
}

// groupRenditions returns the renditions of type t belonging to group (all
// of type t when group is empty and the variant declares none).
func (m Master) groupRenditions(t, group string) []Rendition {
	var out []Rendition
	for _, r := range m.Renditions {
		if r.Type != t {
			continue
		}
		if group == "" || r.Group == group {
			out = append(out, r)
		}
	}
	return out
}

// ─── preferred hours window ──────────────────────────────────────────────────

// window is a daily [start, end) interval in minutes since midnight; it may
// wrap midnight (start > end).
type window struct{ start, end int }

func parseWindow(s string) (window, bool) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return window{}, false
	}
	parse := func(x string) (int, bool) {
		h, m, ok := strings.Cut(strings.TrimSpace(x), ":")
		if !ok {
			return 0, false
		}
		hh, e1 := strconv.Atoi(h)
		mm, e2 := strconv.Atoi(m)
		if e1 != nil || e2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
			return 0, false
		}
		return hh*60 + mm, true
	}
	s1, ok1 := parse(a)
	e1, ok2 := parse(b)
	if !ok1 || !ok2 || s1 == e1 {
		return window{}, false
	}
	return window{s1, e1}, true
}

func (w window) contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end
}

// nextStart is the next time (≥ t) the window opens, or t itself if open now.
func (w window) nextStart(t time.Time) time.Time {
	if w.contains(t) {
		return t
	}
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	start := day.Add(time.Duration(w.start) * time.Minute)
	if !start.After(t) {
		start = start.AddDate(0, 0, 1)
	}
	return start
}

// ─── media playlist → local files ────────────────────────────────────────────
// The downloader fetches every segment itself (so a job can pause and resume
// segment by segment) and then hands ffmpeg a *local* copy of each media
// playlist pointing at the files on disk.

// MediaSegment is one segment and the tags that precede it.
type MediaSegment struct {
	URI      string
	Duration float64
	Tags     []string // verbatim, URIs of EXT-X-KEY / EXT-X-MAP still remote
}

// MediaPlaylist is a parsed media playlist.
type MediaPlaylist struct {
	Header    []string // playlist-level tags before the first segment
	Segments  []MediaSegment
	Ended     bool
	Duration  float64
	ByteRange bool // EXT-X-BYTERANGE: not supported by the downloader
}

// ParseMedia parses a media playlist fetched from playlistURL.
func ParseMedia(content, playlistURL string) MediaPlaylist {
	var p MediaPlaylist
	var pending []string
	var dur float64
	seenSegment := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			p.Segments = append(p.Segments, MediaSegment{URI: resolveURI(playlistURL, line), Duration: dur, Tags: pending})
			p.Duration += dur
			pending, dur, seenSegment = nil, 0, true
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			p.Ended = true
			continue
		case strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE:VOD"):
			p.Ended = true
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE"):
			p.ByteRange = true
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			dur, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
		}
		segmentTag := strings.HasPrefix(line, "#EXTINF") || strings.HasPrefix(line, "#EXT-X-KEY") ||
			strings.HasPrefix(line, "#EXT-X-MAP") || strings.HasPrefix(line, "#EXT-X-DISCONTINUITY") ||
			strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME") || strings.HasPrefix(line, "#EXT-X-BYTERANGE")
		if !seenSegment && !segmentTag {
			p.Header = append(p.Header, line)
			continue
		}
		pending = append(pending, line)
	}
	return p
}

// localResource is a remote file the local playlist needs (segment, key or
// fMP4 init section) and its file name in the work directory.
type localResource struct {
	URL  string
	Name string
	Kind string  // "segment" | "key" | "map"
	End  float64 // segments: playlist time where the segment ends
}

// reURIAttr matches a URI attribute however it's quoted (double, single or
// bare): anything left unrewritten would be resolved by ffmpeg against the
// work directory.
var reURIAttr = regexp.MustCompile(`URI=(?:"([^"]*)"|'([^']*)'|([^,\s"']+))`)

// uriAttrValue is the URI inside one reURIAttr match.
func uriAttrValue(m string) string {
	sm := reURIAttr.FindStringSubmatch(m)
	for _, v := range sm[1:] {
		if v != "" {
			return v
		}
	}
	return ""
}

// Localize rewrites p for files in the work directory, named with prefix
// (e.g. "i0"): segments become "<prefix>-s00012.seg", keys
// "<prefix>-k003.key", init sections "<prefix>-m001.seg". It returns the
// local playlist text and every resource to fetch, in playlist order.
func (p MediaPlaylist) Localize(prefix, playlistURL string) (string, []localResource) {
	var b strings.Builder
	var res []localResource
	named := map[string]string{}
	name := func(u, kind string) string {
		if n, ok := named[kind+"\x00"+u]; ok {
			return n
		}
		count := 0
		for k := range named {
			if strings.HasPrefix(k, kind+"\x00") {
				count++
			}
		}
		n := fmt.Sprintf("%s-%s%03d", prefix, kind[:1], count)
		if kind == "key" {
			n += ".key"
		} else {
			n += ".seg"
		}
		named[kind+"\x00"+u] = n
		res = append(res, localResource{URL: u, Name: n, Kind: kind})
		return n
	}
	for _, h := range p.Header {
		if strings.Contains(h, "URI=") {
			continue // see the tag loop below: never left pointing anywhere
		}
		b.WriteString(h + "\n")
	}
	var t float64
	for i, s := range p.Segments {
		for _, tag := range s.Tags {
			if strings.HasPrefix(tag, "#EXT-X-KEY") || strings.HasPrefix(tag, "#EXT-X-MAP") {
				kind := "key"
				if strings.HasPrefix(tag, "#EXT-X-MAP") {
					kind = "map"
				}
				tag = reURIAttr.ReplaceAllStringFunc(tag, func(m string) string {
					u := resolveURI(playlistURL, uriAttrValue(m))
					return `URI="` + name(u, kind) + `"`
				})
			} else if strings.Contains(tag, "URI=") {
				// Any other tag pointing somewhere (session data, LL-HLS
				// hints, …): ffmpeg doesn't need it and must not follow it.
				continue
			}
			b.WriteString(tag + "\n")
		}
		t += s.Duration
		n := fmt.Sprintf("%s-s%05d.seg", prefix, i)
		res = append(res, localResource{URL: s.URI, Name: n, Kind: "segment", End: t})
		b.WriteString(n + "\n")
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String(), res
}
