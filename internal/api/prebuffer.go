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
		body, ferr := fetchSegment(ctx, segClient, segURL, opts)
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
		// segment fetch will fail too — surface it so ResolveStream can fail
		// fast with an actionable message instead of handing the player a dead
		// stream. media_handler.challengeDomainFromErr keys on "CHALLENGE:".
		if strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge") {
			dom := ""
			if u, e := url.Parse(targetURL); e == nil {
				dom = registrableHost(u.Hostname())
			}
			managers.RecordChallenge(dom, opts.Egress)
			return nil, fmt.Errorf("CHALLENGE:%s prebuffer upstream %d", dom, resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// fetchSegment fetches one media segment and returns its bytes already
// normalized (image-wrapper header stripped) so ProxySegment can serve a warm
// hit verbatim.
func fetchSegment(ctx context.Context, client *http.Client, segURL string, opts managers.PrefetchOptions) ([]byte, error) {
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
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "text/") || strings.HasPrefix(ct, "application/json") {
		return nil, fmt.Errorf("non-video content-type %q", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, segFetchLimit))
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "binary/") {
		if off := tsSyncOffset(body); off > 0 {
			body = body[off:]
		}
	}
	return body, nil
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

// tsSyncOffset returns the index of the first MPEG-TS packet boundary in b
// (a 0x47 sync byte followed by another exactly 188 bytes later), or -1.
func tsSyncOffset(b []byte) int {
	for i := 0; i+188 < len(b); i++ {
		if b[i] == 0x47 && b[i+188] == 0x47 {
			return i
		}
	}
	return -1
}

// fmp4BoxOffset returns the index of the first ISO-BMFF top-level box relevant
// to an fMP4 segment (ftyp/styp for an init/media segment, moof/sidx/emsg/free
// for a media fragment), scanning a bounded prefix, or -1. A box is
// `<4-byte big-endian size><4-byte ASCII type>`.
func fmp4BoxOffset(b []byte) int {
	types := [][]byte{
		[]byte("ftyp"), []byte("styp"), []byte("moof"),
		[]byte("sidx"), []byte("emsg"), []byte("free"),
	}
	limit := len(b) - 8
	if limit > 4096 {
		limit = 4096
	}
	for i := 0; i <= limit; i++ {
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
