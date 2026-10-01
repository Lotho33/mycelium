package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// proxyDebug enables the verbose proxy logs (full headers, playlist bodies,
// URLs), which carry upstream credentials and tokens.
var proxyDebug = os.Getenv("MYCELIUM_DEBUG") == "1"

// proxyAllowUnsigned disables the /proxy/* signature check (debug only: the
// proxy becomes an open relay).
var proxyAllowUnsigned = os.Getenv("MYCELIUM_PROXY_ALLOW_UNSIGNED") == "1"

// segmentRetryBudget bounds ProxySegment's whole retry loop, so a CDN that
// is slow on every attempt fails in seconds instead of stalling the player
// for minutes. A var so tests can shrink it.
var segmentRetryBudget = 45 * time.Second

// playlistReadLimit caps an upstream HLS playlist body.
const playlistReadLimit = 8 << 20 // 8 MiB

// requireProxySig rejects a /proxy/* request without a valid HMAC and
// reports whether the caller should stop. No-op with
// MYCELIUM_PROXY_ALLOW_UNSIGNED=1 or without a signing key.
func requireProxySig(w http.ResponseWriter, r *http.Request) bool {
	if proxyAllowUnsigned || !core.ProxySignEnabled() {
		return false
	}
	if core.VerifyProxyURL(r.URL.Query()) {
		return false
	}
	log.Printf("[proxy] rejected unsigned/tampered request from %s %s", realIP(r), logURL(r.URL.String()))
	http.Error(w, "bad or missing signature", http.StatusForbidden)
	return true
}

// logURL trims a URL for logging to scheme://host/path: queries carry
// tokens and cookies. Full URLs only with MYCELIUM_DEBUG=1.
func logURL(raw string) string {
	if proxyDebug {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable url)"
	}
	s := u.Path
	if u.Scheme != "" || u.Host != "" {
		// absolute URL; a request URL stays path-only.
		s = u.Scheme + "://" + u.Host + u.Path
	}
	if u.RawQuery != "" {
		s += "?<redacted>"
	}
	return s
}

var reKeyURI = regexp.MustCompile(`URI=["']([^"']+)["']`)

// uriTagTarget says which proxy endpoint the URI= of an HLS tag goes
// through: "key" for small raw resources (keys, session keys/data),
// "segment" for binary media (init sections, LL-HLS parts and preload
// hints), "playlist" for the rest. masterChildURIs relies on it too.
func uriTagTarget(line string) string {
	switch {
	case strings.HasPrefix(line, "#EXT-X-KEY"), strings.HasPrefix(line, "#EXT-X-SESSION-KEY"),
		strings.HasPrefix(line, "#EXT-X-SESSION-DATA"):
		return "key"
	case strings.HasPrefix(line, "#EXT-X-MAP"), strings.HasPrefix(line, "#EXT-X-PART"),
		strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT"):
		return "segment"
	}
	return "playlist"
}

func proxyResolveForPlaylist(r *http.Request, sidRaw, uidRaw string) (newPlaylistURL, newOrigin string) {
	pluginID, sourceID, ok := decodeSid(sidRaw)
	if !ok {
		return "", ""
	}
	resolvedURL, resolvedHeaders, err := reResolveShared(sidRaw, sessionProfile(uidRaw))
	if err != nil {
		log.Printf("[proxy/playlist] re-resolve %s/%s failed: %v", pluginID, sourceID, err)
		return "", ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return buildResolvedPlaylistURL(scheme, r.Host, pluginID, sourceID, resolvedURL, resolvedHeaders, uidRaw)
}

// buildResolvedPlaylistURL mints the signed /proxy/playlist.m3u8 target of a
// re-resolved stream.
func buildResolvedPlaylistURL(scheme, host, pluginID, sourceID, resolvedURL string, resolvedHeaders map[string]string, uidRaw string) (newURL, origin string) {
	// Sniffed header names can be lowercase; the lookups below are canonical.
	resolvedHeaders = canonicalHeaderKeys(resolvedHeaders)

	origin = resolvedHeaders["Referer"]
	if origin == "" {
		origin = resolvedHeaders["Origin"]
	}
	b64enc := func(v string) string {
		return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(v))
	}
	newSid := b64enc(pluginID + "\x00" + sourceID)
	uidParam := ""
	if uidRaw != "" {
		uidParam = "&uid=" + url.QueryEscape(uidRaw)
	}
	vpnSuffix := videoEgressSuffix(pluginID)
	xhdrSuffix := buildXhdrSuffix(b64enc, resolvedHeaders)
	// rr=1 marks an already re-resolved URL: a further failure answers 502
	// instead of re-resolving again (no redirect loop).
	u := fmt.Sprintf("%s://%s/proxy/playlist.m3u8?data=%s&origin=%s&cookies=%s&sid=%s%s%s%s&rr=1",
		scheme, host,
		b64enc(resolvedURL),
		b64enc(origin),
		b64enc(resolvedHeaders["Cookie"]),
		url.QueryEscape(newSid),
		uidParam,
		xhdrSuffix,
		vpnSuffix,
	)
	return core.AppendProxySig(u), origin
}

// ProxyPlaylist fetches an upstream HLS playlist and rewrites every URI in
// it to a signed /proxy URL. Unauthenticated: players call it directly;
// the signature is the credential.
func ProxyPlaylist(w http.ResponseWriter, r *http.Request) {
	if requireProxySig(w, r) {
		return
	}
	q := r.URL.Query()
	dataRaw := q.Get("data")
	originRaw := q.Get("origin")
	cookiesRaw := q.Get("cookies")
	xhdrRaw := q.Get("xhdr")
	sidRaw := q.Get("sid")
	uidRaw := q.Get("uid") // playback session token
	useVPN := q.Get("vpn") == "1"
	egr := q.Get("egr") // egress profile name (see videoEgressSuffix)

	realURL := proxyB64Decode(dataRaw)
	realOrigin := proxyB64Decode(originRaw)
	childIdx := q.Get("vi") // set on children of a master

	if _, ok := claimProxySession(w, uidRaw); !ok {
		return
	}

	// A child playlist already recovered once: go straight to its new upstream.
	origURL := realURL
	if childIdx != "" {
		if rm, ok := lookupPlaylistRemap(origURL); ok {
			realURL, realOrigin = rm.url, rm.origin
			originRaw = b64url(rm.origin)
			cookiesRaw, xhdrRaw = rm.cookiesRaw, rm.xhdrRaw
		}
	}

	if proxyDebug { // debug only: every live reload
		log.Printf("[proxy/playlist] url=%s origin=%s uid=%s xhdr_present=%v vpn=%v egr=%s", logURL(realURL), realOrigin, uidRaw, xhdrRaw != "", useVPN, egr)
	}

	plClient, clErr := upstreamPlaylistClient(useVPN, egr)
	if clErr != nil {
		log.Printf("[proxy/playlist] %v url=%s", clErr, logURL(realURL))
		http.Error(w, clErr.Error(), http.StatusBadGateway)
		return
	}

	// Warm-cache hit from the ResolveStream pre-buffer: skip the upstream fetch
	// (the rewrite below still runs).
	var content []byte
	var fetchErr error
	if warm, ok := managers.Segments.Get(realURL); ok {
		content = warm
		// Serve the pre-buffered copy once, then drop it: a live playlist's window
		// slides, so the next reload must come from upstream.
		managers.Segments.Delete(realURL)
		log.Printf("[proxy/playlist] warm-cache hit (%d bytes), evicted for reload url=%s", len(warm), logURL(realURL))
	} else {
		// Concurrent requests for the same URL share one upstream fetch.
		var shared bool
		content, fetchErr, shared = playlistInflight.Do(r.Context(), realURL, func() ([]byte, error) {
			return fetchUpstreamPlaylist(plClient, realURL, realOrigin, cookiesRaw, xhdrRaw, useVPN, egr)
		})
		if shared && proxyDebug {
			log.Printf("[proxy/playlist] shared fetch for %s", logURL(realURL))
		}
	}

	if fetchErr == nil && content != nil {
		segs := strings.Count(string(content), "#EXTINF")
		if proxyDebug {
			log.Printf("[proxy/playlist] fetched %d bytes, %d segments:\n%s", len(content), segs, string(content))
		}
	}

	if fetchErr != nil && sidRaw != "" && childIdx != "" && r.Context().Err() == nil {
		log.Printf("[proxy/playlist] child vi=%s failed (%v), re-resolving", childIdx, fetchErr)
		if rm, recovered, err := recoverChildPlaylist(plClient, sidRaw, sessionProfile(uidRaw), childIdx, origURL, useVPN, egr); err != nil {
			log.Printf("[proxy/playlist] child vi=%s recovery failed: %v", childIdx, err)
		} else {
			log.Printf("[proxy/playlist] child vi=%s recovered → %s", childIdx, logURL(rm.url))
			realURL, realOrigin = rm.url, rm.origin
			originRaw = b64url(rm.origin)
			cookiesRaw, xhdrRaw = rm.cookiesRaw, rm.xhdrRaw
			content, fetchErr = recovered, nil
		}
	}

	if r.Context().Err() != nil {
		return // player hung up
	}
	if fetchErr != nil {
		log.Printf("[proxy/playlist] fetch error: %v", fetchErr)
		if sidRaw != "" && childIdx == "" && q.Get("rr") == "" {
			if newURL, _ := proxyResolveForPlaylist(r, sidRaw, uidRaw); newURL != "" {
				log.Printf("[proxy/playlist] re-resolve → %s", logURL(newURL))
				http.Redirect(w, r, newURL, http.StatusFound)
				return
			}
		}
		http.Error(w, fetchErr.Error(), http.StatusBadGateway)
		return
	}

	playlistBase := realURL[:strings.LastIndex(realURL, "/")+1]
	lines := strings.Split(string(content), "\n")
	b64enc := func(s string) string {
		return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	proxyBase := fmt.Sprintf("%s://%s/proxy", scheme, r.Host)

	uidSuffix := ""
	if uidRaw != "" {
		uidSuffix = "&uid=" + url.QueryEscape(uidRaw)
	}
	xhdrSuffix := ""
	if xhdrRaw != "" {
		xhdrSuffix = "&xhdr=" + url.QueryEscape(xhdrRaw)
	}
	vpnSuffix := childVPNSuffix(q)

	// Children of a master carry sid + their index so a failing variant reload
	// can be recovered in place (proxy_playlist_recover.go).
	master := sidRaw != "" && isMasterPlaylist(content)
	nextChild := 0
	childSuffix := func() string {
		if !master {
			return ""
		}
		sfx := "&sid=" + url.QueryEscape(sidRaw) + "&vi=" + strconv.Itoa(nextChild)
		nextChild++
		return sfx
	}

	var rewritten []string
	var lastExtInf float64
	// After #EXT-X-KEY the segments are ciphertext, whatever their extension or
	// Content-Type: ProxySegment must relay them without sniffing.
	encrypted := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "#EXT-X-KEY") {
				encrypted = keyLineEncrypts(line)
			}
			// Track segment duration for progress estimation.
			if strings.HasPrefix(line, "#EXTINF:") {
				rest := strings.TrimPrefix(line, "#EXTINF:")
				if idx := strings.IndexAny(rest, ",\r\n"); idx >= 0 {
					rest = rest[:idx]
				}
				lastExtInf, _ = strconv.ParseFloat(rest, 64)
			}
			if strings.Contains(line, "URI=") {
				if m := reKeyURI.FindStringSubmatch(line); len(m) == 2 {
					absURI := proxyURLJoin(playlistBase, m[1])
					encURI := b64enc(absURI)
					var replacement string
					switch uriTagTarget(line) {
					case "key":
						replacement = fmt.Sprintf("%s/key.key?data=%s&origin=%s&cookies=%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					case "segment":
						// Init sections and parts are binary: they go through the segment proxy.
						replacement = fmt.Sprintf("%s/segment.ts?data=%s&origin=%s&cookies=%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					default:
						replacement = fmt.Sprintf("%s/playlist.m3u8?data=%s&origin=%s&cookies=%s%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix, childSuffix())
					}
					line = strings.Replace(line, m[1], core.AppendProxySig(replacement), 1)
				}
			}
			rewritten = append(rewritten, line)
		} else {
			// Segment URL: update the session's segment duration.
			if uidRaw != "" && lastExtInf > 0 {
				if s, ok := managers.Sessions.Get(uidRaw); ok {
					s.UpdateSegDuration(lastExtInf)
				}
			}
			lastExtInf = 0

			absURI := proxyURLJoin(playlistBase, line)
			encURI := b64enc(absURI)
			endpoint := "segment.ts"
			lower := strings.ToLower(absURI)
			if strings.Contains(lower, ".m3u8") || strings.Contains(lower, "/playlist/") {
				endpoint = "playlist.m3u8"
			}
			encSuffix := ""
			if encrypted && endpoint == "segment.ts" {
				encSuffix = "&enc=1"
			}
			if master {
				// Every URI line of a master is a variant playlist.
				endpoint = "playlist.m3u8"
				encSuffix = childSuffix()
			}
			rewritten = append(rewritten, core.AppendProxySig(fmt.Sprintf("%s/%s?data=%s&origin=%s&cookies=%s%s%s%s%s",
				proxyBase, endpoint, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix, encSuffix)))
		}
	}

	out := strings.Join(rewritten, "\n")
	if proxyDebug {
		log.Printf("[proxy/playlist] rewritten (%d lines):\n%s", len(rewritten), out)
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(w, out)
}

// ProxySegment relays one media segment (or init section) from upstream.
// Unauthenticated: players call it directly; the signature is the
// credential.
func ProxySegment(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if proxyDebug { // debug only: one line per segment
		log.Printf("[proxy/segment] INCOMING from=%s url=%s", r.RemoteAddr, logURL(r.URL.String()))
	}
	if requireProxySig(w, r) {
		return
	}
	q := r.URL.Query()
	realURL := proxyB64Decode(q.Get("data"))
	realOrigin := proxyB64Decode(q.Get("origin"))
	uidRaw := q.Get("uid")
	useVPN := q.Get("vpn") == "1"
	egr := q.Get("egr")
	encrypted := q.Get("enc") == "1"

	sess, ok := claimProxySession(w, uidRaw)
	if !ok {
		return
	}

	// Warm-cache hit from the ResolveStream pre-buffer.
	if body, ok := managers.Segments.Get(realURL); ok {
		ct := "video/MP2T"
		if lu := strings.ToLower(realURL); strings.Contains(lu, ".mp4") || strings.Contains(lu, ".m4s") || strings.Contains(lu, ".cmfv") || strings.Contains(lu, ".cmfa") {
			ct = "video/mp4"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if proxyDebug {
			log.Printf("[proxy/segment] warm-cache hit (%d bytes) url=%s", len(body), logURL(realURL))
		}
		if _, err := w.Write(body); err != nil && r.Context().Err() != nil {
			log.Printf("[proxy/segment] client disconnected on warm hit after %s url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
		}
		if sess != nil {
			sess.RecordFetch()
		}
		return
	}

	segClient, clErr := upstreamSegmentClient(useVPN, egr)
	if clErr != nil {
		log.Printf("[proxy/segment] %v url=%s", clErr, logURL(realURL))
		http.Error(w, clErr.Error(), http.StatusBadGateway)
		return
	}

	// Cap concurrent upstream fetches per CDN host, so a player's start-of-
	// stream burst doesn't trip a rate-limiting CDN.
	var segHost string
	if u, perr := url.Parse(realURL); perr == nil {
		segHost = u.Host
	}
	gateAcquired, gerr := segAcquire(r.Context(), segHost)
	if gerr != nil {
		log.Printf("[proxy/segment] client gone while queued url=%s", logURL(realURL))
		return
	}
	// Released as soon as the segment is buffered, so a slow player never holds
	// a CDN slot shared with other viewers.
	gateRelease := sync.OnceFunc(gateAcquired)
	defer gateRelease()

	// Retries happen only before anything is written to the player. budgetCtx
	// bounds the whole loop (see segmentRetryBudget), not each attempt.
	budgetCtx, cancel := context.WithTimeout(r.Context(), segmentRetryBudget)
	defer cancel()

	const maxAttempts = 4
	var resp *http.Response
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// A fresh request per attempt (net/http doesn't allow reusing one).
		req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, q.Get("cookies"), q.Get("xhdr"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Cancelled when the player disconnects or the retry budget is spent.
		req = req.WithContext(budgetCtx)

		var doErr error
		resp, doErr = segClient.Do(req)
		doErr = core.RedactURLError(doErr)
		if doErr != nil {
			if r.Context().Err() != nil {
				// The player gave up before upstream answered.
				log.Printf("[proxy/segment] client disconnected after %s waiting on upstream (no response yet) url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
				return
			}
			if budgetCtx.Err() != nil {
				// Retry budget spent: every further attempt would fail at once.
				log.Printf("[proxy/segment] retry budget (%s) exhausted after %s url=%s", segmentRetryBudget, time.Since(start).Round(time.Millisecond), logURL(realURL))
				http.Error(w, "upstream timeout", http.StatusBadGateway)
				return
			}
			if attempt < maxAttempts {
				log.Printf("[proxy/segment] fetch error (attempt %d/%d) url=%s: %v", attempt, maxAttempts, logURL(realURL), doErr)
				select {
				case <-time.After(retryBackoff(attempt)):
				case <-budgetCtx.Done():
					http.Error(w, "upstream timeout", http.StatusBadGateway)
					return
				}
				continue
			}
			log.Printf("[proxy/segment] fetch error url=%s: %v", logURL(realURL), doErr)
			http.Error(w, doErr.Error(), http.StatusBadGateway)
			return
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Record a CDN challenge on the segment itself (see noteChallengeIfAny).
			noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
			resp.Body.Close()
			if attempt < maxAttempts {
				d := segRetryBackoff(attempt, resp.StatusCode)
				log.Printf("[proxy/segment] upstream %d (attempt %d/%d, wait %s) url=%s", resp.StatusCode, attempt, maxAttempts, d, logURL(realURL))
				select {
				case <-time.After(d):
				case <-budgetCtx.Done():
					// Player gone or budget spent: stop retrying.
					return
				}
				continue
			}
			log.Printf("[proxy/segment] upstream %d url=%s", resp.StatusCode, logURL(realURL))
			http.Error(w, fmt.Sprintf("upstream %d", resp.StatusCode), http.StatusBadGateway)
			return
		}

		// Buffer the segment before answering: a connection dropping halfway is
		// still retryable, and the CDN slot is released before the (possibly slow)
		// write to the player. Bodies over segmentBufferLimit are streamed.
		buf, complete, rerr := readSegmentBounded(resp.Body, segmentBufferLimit)
		if rerr != nil {
			resp.Body.Close()
			if r.Context().Err() != nil {
				log.Printf("[proxy/segment] client disconnected after %s while reading upstream url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
				return
			}
			if attempt < maxAttempts && budgetCtx.Err() == nil {
				log.Printf("[proxy/segment] upstream body cut after %d bytes (attempt %d/%d) url=%s: %v", len(buf), attempt, maxAttempts, logURL(realURL), rerr)
				select {
				case <-time.After(retryBackoff(attempt)):
				case <-budgetCtx.Done():
					http.Error(w, "upstream timeout", http.StatusBadGateway)
					return
				}
				continue
			}
			log.Printf("[proxy/segment] upstream body read error url=%s: %v", logURL(realURL), rerr)
			http.Error(w, "segment read error", http.StatusBadGateway)
			return
		}
		if complete {
			resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(buf))
			gateRelease()
		} else {
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(buf), resp.Body), resp.Body}
		}
		break
	}
	defer resp.Body.Close()

	// Keep the CDN's Content-Type (fMP4 segments must be video/mp4).
	ct := resp.Header.Get("Content-Type")
	bodyModified := false
	switch {
	case encrypted:
		// Ciphertext (HLS AES-128): relay it untouched, unless it is plainly a text
		// page (an error or challenge, not a segment).
		head := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, head)
		head = head[:n]
		if looksLikeTextPage(head) {
			log.Printf("[proxy/segment] encrypted segment but body is a text page (content-type=%q) — rejecting url=%s", ct, logURL(realURL))
			noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
			http.Error(w, "non-video segment", http.StatusBadGateway)
			return
		}
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), resp.Body))
		ct = "video/MP2T"
		if lu := strings.ToLower(realURL); strings.Contains(lu, ".mp4") || strings.Contains(lu, ".m4s") || strings.Contains(lu, ".cmfv") || strings.Contains(lu, ".cmfa") {
			ct = "video/mp4"
		}
	case ct == "":
		ct = "video/MP2T"
	case strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "application/"):
		// correct type, pass through
	case strings.HasPrefix(ct, "audio/") || strings.HasPrefix(ct, "text/vtt"):
		// Separate audio renditions (AAC/MP3, often ID3-tagged) and WebVTT
		// subtitles: neither TS nor fMP4, but legitimate.
	case strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "binary/"):
		// Some CDNs wrap segments in an image container. Strict demuxers need the
		// media at offset 0: find where it starts (TS or fMP4) and serve from
		// there; if nothing is found, pass the bytes through.
		sniff, off, mediaCT, serr := sniffMediaStart(resp.Body)
		if serr != nil {
			log.Printf("[proxy/segment] content-type=%q read error: %v url=%s", ct, serr, logURL(realURL))
			http.Error(w, "segment read error", http.StatusBadGateway)
			return
		}
		if off > 0 {
			log.Printf("[proxy/segment] content-type=%q media at offset %d — stripping wrapper url=%s", ct, off, logURL(realURL))
			sniff = sniff[off:]
			ct = mediaCT
		} else if off == 0 {
			ct = mediaCT
		} else {
			log.Printf("[proxy/segment] content-type=%q no media signature in first %d bytes — passing raw bytes url=%s", ct, len(sniff), logURL(realURL))
			ct = "video/MP2T"
		}
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(sniff), resp.Body))
		bodyModified = true
	default:
		// Non-media Content-Type (text/html, mislabelled octet-stream, …): relay it
		// only if the payload really contains media, else reject it as an error
		// page.
		sniff, off, mediaCT, serr := sniffMediaStart(resp.Body)
		if serr != nil {
			log.Printf("[proxy/segment] content-type=%q read error: %v url=%s", ct, serr, logURL(realURL))
			http.Error(w, "segment read error", http.StatusBadGateway)
			return
		}
		if off < 0 {
			if otherCT := nonVideoSegmentType(sniff); otherCT != "" {
				// Mislabelled audio/subtitle segment.
				resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(sniff), resp.Body))
				w.Header().Set("Content-Type", otherCT)
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				if _, err := io.Copy(w, resp.Body); err != nil && r.Context().Err() == nil {
					log.Printf("[proxy/segment] stream error: %v", err)
				}
				if sess != nil {
					sess.RecordFetch()
				}
				return
			}
			preview := ""
			if os.Getenv("MYCELIUM_DEBUG") == "1" {
				n := len(sniff)
				if n > 400 {
					n = 400
				}
				preview = " body[:400]=" + strconv.Quote(string(sniff[:n]))
			}
			log.Printf("[proxy/segment] content-type=%q no media signature in %d bytes — rejecting url=%s%s", ct, len(sniff), logURL(realURL), preview)
			// Challenges are often served as 200 with an HTML body: record it here.
			noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
			http.Error(w, "non-video segment", http.StatusBadGateway)
			return
		}
		if off > 0 {
			log.Printf("[proxy/segment] content-type=%q media at offset %d — stripping wrapper url=%s", ct, off, logURL(realURL))
			sniff = sniff[off:]
		} else {
			log.Printf("[proxy/segment] content-type=%q mislabelled media, relaying as %s url=%s", ct, mediaCT, logURL(realURL))
		}
		ct = mediaCT
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(sniff), resp.Body))
		bodyModified = true
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if !bodyModified {
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			w.Header().Set("Content-Length", cl)
		}
	}
	if proxyDebug {
		log.Printf("[proxy/segment] ok url=%s", logURL(realURL))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		if r.Context().Err() != nil {
			log.Printf("[proxy/segment] client disconnected mid-stream after %s url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
		} else {
			log.Printf("[proxy/segment] stream error: %v", err)
		}
	}

	// Update the session's position estimate.
	if sess != nil {
		sess.RecordFetch()
	}
}

// nonVideoSegmentType recognises the non-video segments a media playlist can
// point at — WebVTT subtitles and AAC/MP3 audio (optionally behind an ID3
// tag) — returning the Content-Type to relay them with, or "".
func nonVideoSegmentType(b []byte) string {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // UTF-8 BOM
	switch {
	case bytes.HasPrefix(b, []byte("WEBVTT")):
		return "text/vtt"
	case bytes.HasPrefix(b, []byte("ID3")):
		return "audio/aac"
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xF6 == 0xF0:
		return "audio/aac" // ADTS
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return "audio/mpeg"
	}
	return ""
}

// segmentBufferLimit caps how much of a segment is buffered before
// answering; larger bodies are streamed.
const segmentBufferLimit = 32 << 20

// readSegmentBounded reads body up to limit bytes. complete=true means the
// whole body is in buf; false means buf holds the first limit bytes and the
// rest is still unread in body.
func readSegmentBounded(body io.Reader, limit int) (buf []byte, complete bool, err error) {
	buf, err = io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return buf, false, err
	}
	if len(buf) > limit {
		return buf, false, nil
	}
	return buf, true, nil
}

// ProxyKey relays an HLS decryption key from upstream. Unauthenticated:
// players call it directly; the signature is the credential.
func ProxyKey(w http.ResponseWriter, r *http.Request) {
	if requireProxySig(w, r) {
		return
	}
	q := r.URL.Query()
	realURL := proxyB64Decode(q.Get("data"))
	realOrigin := proxyB64Decode(q.Get("origin"))
	useVPN := q.Get("vpn") == "1"
	egr := q.Get("egr")

	if _, ok := claimProxySession(w, q.Get("uid")); !ok {
		return
	}

	req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, q.Get("cookies"), q.Get("xhdr"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Cancelled when the player disconnects.
	req = req.WithContext(r.Context())

	keyClient, clErr := upstreamPlaylistClient(useVPN, egr)
	if clErr != nil {
		log.Printf("[proxy/key] %v url=%s", clErr, logURL(realURL))
		http.Error(w, clErr.Error(), http.StatusBadGateway)
		return
	}
	resp, err := keyClient.Do(req)
	if err != nil {
		err = core.RedactURLError(err)
		if r.Context().Err() == nil {
			log.Printf("[proxy/key] fetch error url=%s: %v", logURL(realURL), err)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Record a CDN challenge on the key fetch too.
		noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
		log.Printf("[proxy/key] upstream %d url=%s", resp.StatusCode, logURL(realURL))
		http.Error(w, fmt.Sprintf("upstream %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	// An AES-128 key is 16 bytes; cap generously against an unbounded body.
	const keyReadLimit = 1 << 20 // 1 MiB
	data, err := io.ReadAll(io.LimitReader(resp.Body, keyReadLimit))
	if err != nil {
		log.Printf("[proxy/key] read error: %v", err)
		http.Error(w, "errore lettura chiave upstream", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(data)
}
