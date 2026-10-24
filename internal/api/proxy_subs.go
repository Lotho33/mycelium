package api

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// External subtitles: a plugin's resolve_stream may return `subtitles`
// (core.ExtSubtitle) for tracks the upstream doesn't carry — e.g. Italian
// subtitles for a film only available in English. They travel signed in the
// playlist URL (xsub) and become a TYPE=SUBTITLES rendition of the master,
// served by /proxy/subtitle.m3u8 (a one-segment playlist) and
// /proxy/subtitle.vtt (the file, fetched server-side and turned into WebVTT).
// Players and the offline downloads then treat them as native tracks.

// extSubsGroup is the rendition group used when the master has none.
const extSubsGroup = "mycelium-xsub"

// subtitleReadLimit caps an upstream subtitle file (zip included).
const subtitleReadLimit = 4 << 20

var (
	reSubtitlesAttr = regexp.MustCompile(`SUBTITLES="([^"]*)"`)
	reLanguageAttr  = regexp.MustCompile(`LANGUAGE="([^"]*)"`)
	reNameAttr      = regexp.MustCompile(`NAME="([^"]*)"`)
)

// subLangAliases folds ISO 639-2 codes to 639-1 for the dedup below.
var subLangAliases = map[string]string{
	"ita": "it", "eng": "en", "spa": "es", "fre": "fr", "fra": "fr",
	"ger": "de", "deu": "de", "por": "pt", "pob": "pt", "jpn": "ja",
}

func normSubLang(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if i := strings.IndexAny(l, "-_"); i > 0 {
		l = l[:i]
	}
	if a, ok := subLangAliases[l]; ok {
		return a
	}
	// A playlist attribute value: letters only (ISO 639 codes).
	if len(l) > 8 || strings.Trim(l, "abcdefghijklmnopqrstuvwxyz") != "" {
		return ""
	}
	return l
}

// m3uAttrText makes s safe inside a quoted playlist attribute: no control
// characters (a newline would start a new tag) and no double quote.
func m3uAttrText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return ' '
		case r == '"':
			return '\''
		}
		return r
	}, s)
}

// injectExtSubs adds subs to a rewritten master playlist. A track whose
// language the master already offers (non-forced) is skipped: the upstream's
// own subtitles are synced to this exact video. Without a subtitle group the
// variants get SUBTITLES="mycelium-xsub".
func injectExtSubs(lines []string, subs []core.ExtSubtitle, proxyBase string) []string {
	group := ""
	haveLang := map[string]bool{}
	haveName := map[string]bool{}
	hasDefault := false
	firstVariant := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "#EXT-X-STREAM-INF:") {
			if firstVariant < 0 {
				firstVariant = i
			}
			if m := reSubtitlesAttr.FindStringSubmatch(l); m != nil && group == "" {
				group = m[1]
			}
		}
		if strings.HasPrefix(l, "#EXT-X-MEDIA:") && strings.Contains(l, "TYPE=SUBTITLES") {
			if m := reNameAttr.FindStringSubmatch(l); m != nil {
				haveName[m[1]] = true
			}
			if strings.Contains(l, "FORCED=YES") {
				continue
			}
			if m := reLanguageAttr.FindStringSubmatch(l); m != nil {
				haveLang[normSubLang(m[1])] = true
			}
			if strings.Contains(l, "DEFAULT=YES") {
				hasDefault = true
			}
		}
	}
	if firstVariant < 0 {
		return lines
	}
	addGroup := group == ""
	if addGroup {
		group = extSubsGroup
	}

	var media []string
	for _, s := range subs {
		lang := normSubLang(s.Lang)
		if lang != "" && haveLang[lang] {
			continue
		}
		haveLang[lang] = true
		name := s.Name
		if name == "" {
			name = strings.ToUpper(lang)
		}
		for base, n := name, 2; haveName[name]; n++ {
			name = fmt.Sprintf("%s (%d)", base, n)
		}
		haveName[name] = true
		def := "NO"
		if s.Default && !hasDefault {
			def, hasDefault = "YES", true
		}
		uri := core.AppendProxySig(proxyBase + "/subtitle.m3u8?data=" + b64url(s.URL))
		line := fmt.Sprintf(`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="%s",NAME="%s",DEFAULT=%s,AUTOSELECT=YES`,
			group, m3uAttrText(name), def)
		if lang != "" {
			line += fmt.Sprintf(`,LANGUAGE="%s"`, lang)
		}
		media = append(media, line+`,URI="`+uri+`"`)
	}
	if len(media) == 0 {
		return lines
	}

	out := make([]string, 0, len(lines)+len(media))
	for i, l := range lines {
		if i == firstVariant {
			out = append(out, media...)
		}
		if addGroup && strings.HasPrefix(l, "#EXT-X-STREAM-INF:") && !reSubtitlesAttr.MatchString(l) {
			l += `,SUBTITLES="` + group + `"`
		}
		out = append(out, l)
	}
	return out
}

// serveExtSubsWrapper answers a top-level media playlist that carries xsub
// with a one-variant master: the subtitle renditions plus the same playlist
// URL without xsub. The body just fetched goes back into the warm cache so
// the player's follow-up request doesn't hit upstream again.
func serveExtSubsWrapper(w http.ResponseWriter, r *http.Request, proxyBase, realURL string, content []byte, subs []core.ExtSubtitle) {
	managers.Segments.Put(realURL, content)
	q := r.URL.Query()
	q.Del("xsub")
	q.Del("sig")
	child := core.AppendProxySig(proxyBase + "/playlist.m3u8?" + q.Encode())
	lines := []string{"#EXTM3U", "#EXT-X-STREAM-INF:BANDWIDTH=2000000", child}
	out := strings.Join(injectExtSubs(lines, subs, proxyBase), "\n")
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(w, out)
}

// ─── subtitle files ──────────────────────────────────────────────────────────

type vttEntry struct {
	vtt []byte
	dur float64 // end of the last cue, seconds
	at  time.Time
}

// vttCache keeps the converted files between the playlist and file requests
// (and across seeks/reopens). Small: a few hundred KB per entry.
var vttCache = struct {
	sync.Mutex
	m map[string]vttEntry
}{m: map[string]vttEntry{}}

const (
	vttCacheMax = 32
	vttCacheTTL = 6 * time.Hour
)

// emptyVTTDur is the duration announced for a subtitle that couldn't be
// fetched: an empty but valid track, so a failing subtitle never breaks
// playback.
const emptyVTTDur = 6 * 3600

func loadVTT(ctx context.Context, rawURL string) (vttEntry, error) {
	vttCache.Lock()
	e, ok := vttCache.m[rawURL]
	vttCache.Unlock()
	if ok && time.Since(e.at) < vttCacheTTL {
		return e, nil
	}
	body, err := fetchSubtitle(ctx, rawURL)
	if err != nil {
		return vttEntry{}, err
	}
	vtt, err := toWebVTT(body)
	if err != nil {
		return vttEntry{}, err
	}
	e = vttEntry{vtt: vtt, dur: vttDuration(vtt), at: time.Now()}
	vttCache.Lock()
	if len(vttCache.m) >= vttCacheMax {
		var oldK string
		var oldT time.Time
		for k, v := range vttCache.m {
			if oldK == "" || v.at.Before(oldT) {
				oldK, oldT = k, v.at
			}
		}
		delete(vttCache.m, oldK)
	}
	vttCache.m[rawURL] = e
	vttCache.Unlock()
	return e, nil
}

func fetchSubtitle(ctx context.Context, rawURL string) ([]byte, error) {
	if strings.HasPrefix(rawURL, core.InlineSubtitleScheme) {
		if b, ok := core.InlineSubtitle(rawURL); ok {
			return b, nil
		}
		return nil, fmt.Errorf("inline subtitle no longer in memory")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", proxyUserAgent)
	resp, err := getProxySession().Do(req)
	if err != nil {
		return nil, core.RedactURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, subtitleReadLimit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > subtitleReadLimit {
		return nil, fmt.Errorf("sottotitolo oltre %d MiB", subtitleReadLimit>>20)
	}
	return b, nil
}

var (
	reSRTTime  = regexp.MustCompile(`(\d{1,2}:\d{2}:\d{2}),(\d{3})`)
	reASSTag   = regexp.MustCompile(`\{\\[^}]*\}`)
	reFontTag  = regexp.MustCompile(`(?i)</?font[^>]*>`)
	reCueTimes = regexp.MustCompile(`-->\s*((?:\d+:)?\d{2}:\d{2}[.,]\d{3})`)
)

// toWebVTT unpacks (gzip, zip) and converts an SRT or WebVTT file to UTF-8
// WebVTT.
func toWebVTT(b []byte) ([]byte, error) {
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		b, err = io.ReadAll(io.LimitReader(zr, subtitleReadLimit))
		if err != nil {
			return nil, err
		}
	}
	if bytes.HasPrefix(b, []byte("PK\x03\x04")) {
		var err error
		if b, err = subtitleFromZip(b); err != nil {
			return nil, err
		}
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(b) {
		b = cp1252ToUTF8(b)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if !strings.HasPrefix(strings.TrimSpace(s), "WEBVTT") {
		if !strings.Contains(s, "-->") {
			return nil, fmt.Errorf("formato sottotitolo non riconosciuto")
		}
		var sb strings.Builder
		sb.WriteString("WEBVTT\n\n")
		for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
			if strings.Contains(l, "-->") {
				l = reSRTTime.ReplaceAllString(l, "$1.$2")
			} else {
				l = reFontTag.ReplaceAllString(reASSTag.ReplaceAllString(l, ""), "")
			}
			sb.WriteString(l)
			sb.WriteByte('\n')
		}
		s = sb.String()
	}
	return []byte(s), nil
}

// subtitleFromZip returns the first .srt/.vtt in a zip archive (subtitle
// sites often serve one zipped).
func subtitleFromZip(b []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		n := strings.ToLower(f.Name)
		if !strings.HasSuffix(n, ".srt") && !strings.HasSuffix(n, ".vtt") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		out, err := io.ReadAll(io.LimitReader(rc, subtitleReadLimit))
		rc.Close()
		return out, err
	}
	return nil, fmt.Errorf("nessun .srt/.vtt nello zip")
}

// cp1252Extra maps Windows-1252's 0x80–0x9F block; the rest is Latin-1.
var cp1252Extra = [32]rune{
	'€', 0x81, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0x8d, 'Ž', 0x8f,
	0x90, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0x9d, 'ž', 'Ÿ',
}

func cp1252ToUTF8(b []byte) []byte {
	var sb strings.Builder
	sb.Grow(len(b) + len(b)/8)
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c < 0xa0:
			sb.WriteRune(cp1252Extra[c-0x80])
		default:
			sb.WriteRune(rune(c))
		}
	}
	return []byte(sb.String())
}

// vttDuration is the end time of the last cue, in seconds.
func vttDuration(vtt []byte) float64 {
	maxEnd := 0.0
	for _, m := range reCueTimes.FindAllSubmatch(vtt, -1) {
		if t := parseVTTTime(string(m[1])); t > maxEnd {
			maxEnd = t
		}
	}
	return maxEnd
}

func parseVTTTime(s string) float64 {
	s = strings.ReplaceAll(s, ",", ".")
	parts := strings.Split(s, ":")
	total := 0.0
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0
		}
		total = total*60 + v
	}
	return total
}

// ProxySubtitlePlaylist serves the one-segment media playlist of an external
// subtitle. A file that can't be fetched becomes an empty track instead of an
// error: a broken subtitle must not stop the video.
func ProxySubtitlePlaylist(w http.ResponseWriter, r *http.Request) {
	if requireProxySig(w, r) {
		return
	}
	dataRaw := r.URL.Query().Get("data")
	rawURL := proxyB64Decode(dataRaw)
	dur := float64(emptyVTTDur)
	if e, err := loadVTT(r.Context(), rawURL); err != nil {
		log.Printf("[proxy/subtitle] %s: %v", logURL(rawURL), err)
	} else if e.dur > 0 {
		dur = e.dur + 1
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	file := core.AppendProxySig(fmt.Sprintf("%s://%s/proxy/subtitle.vtt?data=%s", scheme, r.Host, dataRaw))
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:%.3f,\n%s\n#EXT-X-ENDLIST\n",
		int(math.Ceil(dur)), dur, file)
}

// ProxySubtitleFile serves an external subtitle as WebVTT.
func ProxySubtitleFile(w http.ResponseWriter, r *http.Request) {
	if requireProxySig(w, r) {
		return
	}
	rawURL := proxyB64Decode(r.URL.Query().Get("data"))
	if u, err := url.Parse(rawURL); !strings.HasPrefix(rawURL, core.InlineSubtitleScheme) &&
		(err != nil || (u.Scheme != "http" && u.Scheme != "https")) {
		http.Error(w, "bad subtitle url", http.StatusBadRequest)
		return
	}
	body := []byte("WEBVTT\n\n")
	if e, err := loadVTT(r.Context(), rawURL); err != nil {
		log.Printf("[proxy/subtitle] %s: %v", logURL(rawURL), err)
	} else {
		body = e.vtt
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(body)
}
