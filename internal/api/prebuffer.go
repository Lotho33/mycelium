package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// This file implements managers.PrefetchHLSHead — the "pre-buffer the head of
// the stream" step ResolveStream runs before it returns {result}. It lives in
// internal/api (not internal/pileus) because it reuses proxy.go's upstream
// plumbing: buildProxyRequest (browser-baseline headers), the uTLS transport
// clients, and VPN client selection. internal/pileus injects it via the
// managers.PrefetchHLSHead func var, wired here in init().

func init() {
	managers.PrefetchHLSHead = prefetchHLSHead
}

// Upstream client selection, indirected so tests can substitute a client that
// trusts an httptest server instead of the uTLS/DoH transport.
var (
	prefetchPlaylistClientFn = upstreamPlaylistClient
	prefetchSegmentClientFn  = upstreamSegmentClient
)

// segFetchLimit caps how many bytes we read for a single pre-fetched segment,
// a backstop against a mislabelled full-movie MP4 behind an .m3u8. The real
// budget is opts.MaxBytes across the whole run; this is per-object insurance.
const segFetchLimit = 24 << 20 // 24 MiB

func prebufB64(s string) string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
}

// prefetchReq rebuilds the exact request the player's own /proxy call would
// make for targetURL: same baseline headers, same Referer/Cookie, same
// captured extra headers (xhdr). Faithfulness matters — several CDNs 403 a
// request whose header set doesn't match.
func prefetchReq(ctx context.Context, method, targetURL string, opts managers.PrefetchOptions) (*http.Request, error) {
	origin, cookie := "", ""
	if opts.Headers != nil {
		if v := opts.Headers["Referer"]; v != "" {
			origin = v
		} else {
			origin = opts.Headers["Origin"]
		}
		cookie = opts.Headers["Cookie"]
	}
	// buildXhdrSuffix returns "&xhdr=<queryEscaped b64>"; buildProxyRequest
	// wants the bare b64 value.
	xhdrRaw := ""
	if s := buildXhdrSuffix(prebufB64, opts.Headers); s != "" {
		xhdrRaw = strings.TrimPrefix(s, "&xhdr=")
		if u, err := url.QueryUnescape(xhdrRaw); err == nil {
			xhdrRaw = u
		}
	}
	req, err := buildProxyRequest(method, targetURL, origin, prebufB64(cookie), xhdrRaw)
	if err != nil {
		return nil, err
	}
	return req.WithContext(ctx), nil
}

// prefetchHLSHead — see managers.PrefetchHLSHead.
func prefetchHLSHead(ctx context.Context, rawURL string, opts managers.PrefetchOptions, emit func(managers.PrefetchProgress)) managers.PrefetchResult {
	res := managers.PrefetchResult{}
	if opts.MaxSegments <= 0 {
		return res
	}
	if emit == nil {
		emit = func(managers.PrefetchProgress) {}
	}
	// The heartbeat goroutine and the fetch loop both emit; downstream that
	// ends in grpc stream.Send, which is not safe for concurrent use. Funnel
	// every emit through one mutex.
	var emitMu sync.Mutex
	safeEmit := func(p managers.PrefetchProgress) {
		emitMu.Lock()
		emit(p)
		emitMu.Unlock()
	}

	wait := 15 * time.Second
	if opts.MaxWait > 0 {
		wait = opts.MaxWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	plClient, err := prefetchPlaylistClientFn(opts.UseVPN, opts.Egress)
	if err != nil {
		res.Err = err
		return res
	}
	segClient, err := prefetchSegmentClientFn(opts.UseVPN, opts.Egress)
	if err != nil {
		res.Err = err
		return res
	}

	// ── 1. playlist (follow one master→variant hop, max BANDWIDTH) ──────────
	safeEmit(managers.PrefetchProgress{Phase: "playlist"})

	topBody, err := fetchBody(ctx, plClient, rawURL, opts, 4<<20)
	if err != nil {
		res.Err = fmt.Errorf("playlist fetch: %w", err)
		res.TimedOut = errors.Is(err, context.DeadlineExceeded)
		return res
	}
	managers.Segments.Put(rawURL, topBody)

	mediaURL, mediaBody := rawURL, topBody
	if isMasterPlaylist(topBody) {
		variantURL := bestVariant(rawURL, topBody)
		if variantURL == "" {
			res.Err = errors.New("master playlist: no variant")
			return res
		}
		vb, verr := fetchBody(ctx, plClient, variantURL, opts, 4<<20)
		if verr != nil {
			res.Err = fmt.Errorf("variant fetch: %w", verr)
			res.TimedOut = errors.Is(verr, context.DeadlineExceeded)
			return res
		}
		managers.Segments.Put(variantURL, vb)
		mediaURL, mediaBody = variantURL, vb
	}

	// ── 2. segment list ────────────────────────────────────────────────────
	targets := segmentURLs(mediaURL, mediaBody, opts.MaxSegments)
	encrypted := playlistEncrypted(mediaBody)
	total := len(targets)
	if total == 0 {
		res.Err = errors.New("no segments in playlist")
		return res
	}

	// ── 3. heartbeat: keep progress flowing every few seconds even if one
	// segment is slow, so the client's 30s inactivity timeout can't trip. ──
	prog := progressState{total: total}
	stopHB := make(chan struct{})
	hbDone := make(chan struct{})
	core.SafeGo("prebuffer/heartbeat", func() {
		defer close(hbDone)
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopHB:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if ctx.Err() != nil {
					return
				}
				d, b := prog.snapshot()
				safeEmit(managers.PrefetchProgress{Phase: "segment", Done: d, Total: total, Bytes: b})
			}
		}
	})

	// ── 4. fetch segments sequentially (mirrors how a player ramps up) ─────
	for _, segURL := range targets {
		if ctx.Err() != nil {
			res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
			break
		}
		body, ferr := fetchSegment(ctx, segClient, segURL, opts, encrypted)
		if ferr != nil {
			if ctx.Err() != nil {
				res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
			} else {
				log.Printf("[prebuffer] segment %s: %v (stopping early)", segURL, ferr)
			}
			break
		}
		managers.Segments.Put(segURL, body)
		d, b := prog.add(len(body))
		safeEmit(managers.PrefetchProgress{Phase: "segment", Done: d, Total: total, Bytes: b})
		if opts.MaxBytes > 0 && b >= opts.MaxBytes {
			break
		}
	}

	close(stopHB)
	<-hbDone

	res.Segments, res.Bytes = prog.snapshot()
	safeEmit(managers.PrefetchProgress{Phase: "done", Done: res.Segments, Total: total, Bytes: res.Bytes})
	return res
}

// ─── helpers ────────────────────────────────────────────────────────────────

type progressState struct {
	mu    sync.Mutex
	done  int
	bytes int64
	total int
}

func (p *progressState) add(n int) (int, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done++
	p.bytes += int64(n)
	return p.done, p.bytes
}

func (p *progressState) snapshot() (int, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done, p.bytes
}

func fetchBody(ctx context.Context, client *http.Client, targetURL string, opts managers.PrefetchOptions, limit int64) ([]byte, error) {
	req, err := prefetchReq(ctx, http.MethodGet, targetURL, opts)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// An interactive-verification response on the playlist means every
		// segment fetch will fail too, but there's no special-cased recourse
		// for it any more — same generic failure as any other bad upstream
		// status, falls into the normal best-effort prebuffer fallback.
		if strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge") {
			dom := ""
			if u, e := url.Parse(targetURL); e == nil {
				dom = registrableHost(u.Hostname())
			}
			log.Printf("[prebuffer/verify] %s → upstream wants interactive verification", dom)
		}
		return nil, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// fetchSegment fetches one media segment and returns its bytes already
// normalized (image-wrapper header stripped) so ProxySegment can serve a warm
// hit verbatim.
func fetchSegment(ctx context.Context, client *http.Client, segURL string, opts managers.PrefetchOptions, encrypted bool) ([]byte, error) {
	req, err := prefetchReq(ctx, http.MethodGet, segURL, opts)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	if encrypted {
		// Ciphertext: cache verbatim (ProxySegment serves it the same way, see
		// its `enc=1` path), refusing only an obvious text page.
		body, err := io.ReadAll(io.LimitReader(resp.Body, segFetchLimit))
		if err != nil {
			return nil, err
		}
		if looksLikeTextPage(body) {
			return nil, fmt.Errorf("encrypted segment is a text page (content-type %q)", resp.Header.Get("Content-Type"))
		}
		return body, nil
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/") || ct == "" {
		return io.ReadAll(io.LimitReader(resp.Body, segFetchLimit))
	}
	// Any other content-type (text/html, image/*, binary/*, application/*) may
	// be a wrapper the CDN puts around real MPEG-TS/fMP4 — same disguises
	// ProxySegment unwraps. Look for the media inside before deciding; refusing
	// on content-type alone left every wrapped stream at "0 segs warmed".
	head, off, _, serr := sniffMediaStart(resp.Body)
	if serr != nil {
		return nil, serr
	}
	if off < 0 {
		if strings.HasPrefix(ct, "text/") || strings.HasPrefix(ct, "application/json") {
			return nil, fmt.Errorf("non-video content-type %q", ct)
		}
		off = 0 // image/binary without a detectable signature: keep raw, as before
	}
	rest, err := io.ReadAll(io.LimitReader(resp.Body, segFetchLimit))
	if err != nil {
		return nil, err
	}
	return append(head[off:], rest...), nil
}

func isMasterPlaylist(b []byte) bool {
	return strings.Contains(string(b), "#EXT-X-STREAM-INF")
}

// bestVariant returns the absolute URL of the highest-BANDWIDTH variant in a
// master playlist (libmpv's default hls-bitrate=max, and the same choice
// hub.go makes). "" if none found.
func bestVariant(masterURL string, body []byte) string {
	base := masterURL
	if i := strings.LastIndex(masterURL, "/"); i >= 0 {
		base = masterURL[:i+1]
	}
	lines := strings.Split(string(body), "\n")
	bestURL, bestBW := "", -1
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			continue
		}
		bw := 0
		if idx := strings.Index(line, "BANDWIDTH="); idx >= 0 {
			rest := line[idx+len("BANDWIDTH="):]
			if end := strings.IndexAny(rest, ",\r\n "); end >= 0 {
				rest = rest[:end]
			}
			bw, _ = strconv.Atoi(rest)
		}
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "#") {
				continue
			}
			if bw > bestBW {
				bestBW = bw
				bestURL = proxyURLJoin(base, next)
			}
			break
		}
	}
	return bestURL
}

// segmentURLs parses a media playlist and returns up to max absolute segment
// URLs, with the #EXT-X-MAP init segment (fMP4) first if present.
func segmentURLs(mediaURL string, body []byte, max int) []string {
	base := mediaURL
	if i := strings.LastIndex(mediaURL, "/"); i >= 0 {
		base = mediaURL[:i+1]
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MAP") {
			if m := reKeyURI.FindStringSubmatch(line); len(m) == 2 {
				out = append(out, proxyURLJoin(base, m[1]))
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, proxyURLJoin(base, line))
		if len(out) >= max {
			break
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// tsSyncOffset returns the index of the first MPEG-TS packet boundary in b, or
// -1. A candidate is a 0x47 sync byte repeated at 188-byte stride: at least 3
// consecutive syncs must be confirmed (a 4th too, when the buffer is long
// enough to hold it). Two syncs alone (the old rule) match random data about
// once per 64 KiB, which is a real problem now that wrappers can be large
// binary blobs (fake JPEG/WebP payloads) that we scan through.
func tsSyncOffset(b []byte) int {
	const stride, confirm, maxCheck = 188, 3, 4
	for i := 0; i+(confirm-1)*stride < len(b); i++ {
		if b[i] != 0x47 {
			continue
		}
		ok := true
		for k := 1; k < maxCheck; k++ {
			p := i + k*stride
			if p >= len(b) {
				break
			}
			if b[p] != 0x47 {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// fmp4BoxOffset returns the index of the first ISO-BMFF top-level box relevant
// to an fMP4 segment (ftyp/styp for an init/media segment, moof/sidx/emsg/free
// for a media fragment), or -1. A box is
// `<4-byte big-endian size><4-byte ASCII type>`.
func fmp4BoxOffset(b []byte) int {
	types := [][]byte{
		[]byte("ftyp"), []byte("styp"), []byte("moof"),
		[]byte("sidx"), []byte("emsg"), []byte("free"),
	}
	for i := 0; i+8 <= len(b); i++ {
		// Real box sizes are far below 16 MiB, so the high byte is 0x00.
		// Requiring it keeps ASCII noise (an HTML wrapper that happens to
		// contain the word "free") from being taken for a box header.
		if b[i] != 0 {
			continue
		}
		size := uint32(b[i])<<24 | uint32(b[i+1])<<16 | uint32(b[i+2])<<8 | uint32(b[i+3])
		if size < 8 {
			continue
		}
		for _, t := range types {
			if b[i+4] == t[0] && b[i+5] == t[1] && b[i+6] == t[2] && b[i+7] == t[3] {
				return i
			}
		}
	}
	return -1
}

// mediaStartOffset returns the byte offset where playable media begins in b — a
// wrapper prefix (fake image/JS/HTML header some CDNs prepend, or just noise
// before the real bytes) is skipped. Returns (offset, "video/MP2T") for MPEG-TS,
// (offset, "video/mp4") for fMP4, or (-1, "") when b carries no media signature
// at all (a genuine error page).
func mediaStartOffset(b []byte) (int, string) {
	if off := tsSyncOffset(b); off >= 0 {
		return off, "video/MP2T"
	}
	if off := fmp4BoxOffset(b); off >= 0 {
		return off, "video/mp4"
	}
	return -1, ""
}

// mediaSniffLimit bounds how far into a segment response we look for the real
// media behind a wrapper. Wrapper sizes are not fixed: logs show fake-HTML
// prefixes anywhere from ~6 KB to >64 KB, and fake JPEG/WebP prefixes are
// image-sized. The previous 64 KiB window rejected every video segment whose
// wrapper was larger than that (all 1308 rejections in the 2026-09-11 log).
const mediaSniffLimit = 4 << 20 // 4 MiB

// sniffMediaStart reads r in chunks until it finds a media signature
// (mediaStartOffset) or has consumed mediaSniffLimit bytes / hit EOF. It
// returns everything read so far in buf (the caller must chain it back in front
// of the unread remainder of r), plus the offset and content-type of the media
// inside buf, or off=-1 when none was found. It stops as soon as the media
// start is known, so a normal segment is not buffered beyond its wrapper.
func sniffMediaStart(r io.Reader) (buf []byte, off int, mediaCT string, err error) {
	const chunk = 32 << 10
	// Re-scan a small overlap so a signature straddling two chunks is caught;
	// ts needs 3 packets (2*188 bytes past the candidate) to confirm.
	const overlap = 3*188 + 8
	scanned := 0
	for len(buf) < mediaSniffLimit {
		start := len(buf)
		buf = append(buf, make([]byte, chunk)...)
		n, rerr := io.ReadFull(r, buf[start:])
		buf = buf[:start+n]
		from := scanned - overlap
		if from < 0 {
			from = 0
		}
		if o, ct := mediaStartOffset(buf[from:]); o >= 0 {
			return buf, from + o, ct, nil
		}
		scanned = len(buf)
		if rerr != nil {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				return buf, -1, "", nil
			}
			return buf, -1, "", rerr
		}
	}
	return buf, -1, "", nil
}

// keyLineEncrypts reports whether an #EXT-X-KEY line turns encryption on for
// the segments that follow (METHOD=NONE turns it off).
func keyLineEncrypts(line string) bool {
	return strings.HasPrefix(line, "#EXT-X-KEY") && !strings.Contains(strings.ToUpper(line), "METHOD=NONE")
}

// playlistEncrypted reports whether a media playlist declares an active
// #EXT-X-KEY anywhere.
func playlistEncrypted(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		if keyLineEncrypts(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// looksLikeTextPage reports whether the start of b is an HTML/XML/JSON
// document rather than binary data — what a CDN error or challenge page looks
// like. AES ciphertext (uniformly random) essentially never starts this way.
func looksLikeTextPage(b []byte) bool {
	s := strings.ToLower(strings.TrimLeft(string(b[:min(len(b), 64)]), " \t\r\n\ufeff"))
	for _, p := range []string{"<!doctype", "<html", "<head", "<body", "<script", "<?xml", "{\"", "{ \""} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
