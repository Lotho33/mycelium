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
	"path"
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

// proxiableURI reports whether a playlist URI is fetched through the proxy:
// http(s) and relative references. data:, skd:// (FairPlay) and the like
// stay as they are.
func proxiableURI(u string) bool {
	if i := strings.IndexByte(u, ':'); i > 0 && !strings.ContainsAny(u[:i], "/?#") {
		scheme := strings.ToLower(u[:i])
		return scheme == "http" || scheme == "https"
	}
	return true
}

// requireProxySig rejects a /proxy/* request without a valid HMAC and
// reports whether the caller should stop. No-op only with
// MYCELIUM_PROXY_ALLOW_UNSIGNED=1; without a signing key it fails closed.
func requireProxySig(w http.ResponseWriter, r *http.Request) bool {
	if proxyAllowUnsigned {
		return false
	}
	if core.VerifyProxyURL(path.Base(r.URL.Path), r.URL.Query()) {
		return false
	}
	log.Printf("[proxy] rejected unsigned/tampered/expired request from %s %s", realIP(r), logURL(r.URL.String()))
	http.Error(w, "bad, expired or missing signature", http.StatusForbidden)
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

var reKeyURI = regexp.MustCompile(`(?:^|[:,])URI=["']([^"']+)["']`)

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

func proxyResolveForPlaylist(r *http.Request, sidRaw, uidRaw, xsubRaw string) (newPlaylistURL, newOrigin string) {
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
	return buildResolvedPlaylistURL(scheme, r.Host, pluginID, sourceID, resolvedURL, resolvedHeaders, uidRaw, xsubRaw)
}

// buildResolvedPlaylistURL mints the signed /proxy/playlist.m3u8 target of a
// re-resolved stream. xsubRaw carries the original's external subtitles over.
func buildResolvedPlaylistURL(scheme, host, pluginID, sourceID, resolvedURL string, resolvedHeaders map[string]string, uidRaw, xsubRaw string) (newURL, origin string) {
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
	if xsubRaw != "" {
		vpnSuffix += "&xsub=" + url.QueryEscape(xsubRaw)
	}
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
	// rt=a: an alternate rendition (audio/subtitles/I-frames) of a master —
	// its segments don't count toward the session's position estimate.
	rendition := q.Get("rt") == "a"

	sess, ok := claimProxySession(w, uidRaw)
	if !ok {
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
	liveFetched := false // a live media playlist just fetched upstream
	if warm, ok := managers.Segments.Get(realURL); ok {
		content = warm
		// Serve the pre-buffered copy once, then drop it: a live playlist's window
		// slides, so the next reload must come from upstream.
		managers.Segments.Delete(realURL)
		log.Printf("[proxy/playlist] warm-cache hit (%d bytes), evicted for reload url=%s", len(warm), logURL(realURL))
	} else if live, ok := livePlaylistGet(realURL); ok {
		// Another player of this live stream reloaded it a moment ago.
		content = live
	} else {
		// Concurrent requests for the same URL share one upstream fetch.
		var shared bool
		content, fetchErr, shared = playlistInflight.Do(r.Context(), realURL, func() ([]byte, error) {
			return fetchUpstreamPlaylist(plClient, realURL, realOrigin, cookiesRaw, xhdrRaw, useVPN, egr)
		})
		if shared && proxyDebug {
			log.Printf("[proxy/playlist] shared fetch for %s", logURL(realURL))
		}
		liveFetched = fetchErr == nil && isLiveMediaPlaylist(content)
		if liveFetched {
			livePlaylistPut(realURL, content)
		}
	}

	if proxyDebug && fetchErr == nil && content != nil {
		log.Printf("[proxy/playlist] fetched %d bytes, %d segments:\n%s", len(content), bytes.Count(content, []byte("#EXTINF")), content)
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
			if newURL, _ := proxyResolveForPlaylist(r, sidRaw, uidRaw, q.Get("xsub")); newURL != "" {
				log.Printf("[proxy/playlist] re-resolve → %s", logURL(newURL))
				http.Redirect(w, r, newURL, http.StatusFound)
				return
			}
		}
		http.Error(w, fetchErr.Error(), http.StatusBadGateway)
		return
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	proxyBase := fmt.Sprintf("%s://%s/proxy", scheme, r.Host)

	// External subtitles (xsub) need a master to hang off: a top-level media
	// playlist is wrapped in a one-variant master (proxy_subs.go).
	isMaster := isMasterPlaylist(content)
	extSubs := core.DecodeExtSubs(q.Get("xsub"))
	if len(extSubs) > 0 && !isMaster && childIdx == "" && !rendition {
		serveExtSubsWrapper(w, r, proxyBase, realURL, content, extSubs)
		return
	}

	playlistBase := realURL[:strings.LastIndex(realURL, "/")+1]
	lines := strings.Split(string(content), "\n")
	b64enc := func(s string) string {
		return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
	}

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
	master := sidRaw != "" && isMaster
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
	var raSegs []raSeg // this media playlist's segments, for the read-ahead
	// After #EXT-X-KEY the segments are ciphertext, whatever their extension or
	// Content-Type: ProxySegment must relay them without sniffing.
	encrypted := false
	for li, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if isMaster && strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
				line = fixVariantResolution(line, lines[li+1:], playlistBase)
			}
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
				// m[2]:m[3] is the URI value itself: replace that span, not the first
				// equal substring of the line (it may sit in another attribute).
				if m := reKeyURI.FindStringSubmatchIndex(line); len(m) == 4 && proxiableURI(line[m[2]:m[3]]) {
					absURI := proxyURLJoin(playlistBase, line[m[2]:m[3]])
					encURI := b64enc(absURI)
					var replacement string
					switch uriTagTarget(line) {
					case "key":
						replacement = fmt.Sprintf("%s/key.key?data=%s&origin=%s&cookies=%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					case "segment":
						// Init sections and parts are binary: they go through the segment
						// proxy, but aren't whole segments of playback (nr=1).
						replacement = fmt.Sprintf("%s/segment.ts?data=%s&origin=%s&cookies=%s%s%s%s&nr=1", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					default:
						rtSuffix := ""
						if isMaster {
							// A URI tag in a master names an alternate rendition, never the
							// main variant (those are plain URI lines).
							rtSuffix = "&rt=a"
						}
						replacement = fmt.Sprintf("%s/playlist.m3u8?data=%s&origin=%s&cookies=%s%s%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix, childSuffix(), rtSuffix)
					}
					line = line[:m[2]] + core.AppendProxySig(replacement) + line[m[3]:]
				}
			}
			rewritten = append(rewritten, line)
		} else {
			// Segment URL: update the session's segment duration (main rendition
			// only).
			if sess != nil && lastExtInf > 0 && !rendition {
				sess.UpdateSegDuration(lastExtInf)
			}
			segDur := lastExtInf
			lastExtInf = 0

			if !proxiableURI(line) {
				rewritten = append(rewritten, line) // data:, skd://… are not fetched
				continue
			}
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
			if rendition && endpoint == "segment.ts" {
				encSuffix += "&nr=1"
			}
			if master {
				// Every URI line of a master is a variant playlist.
				endpoint = "playlist.m3u8"
				encSuffix = childSuffix()
			}
			if endpoint == "segment.ts" && !isMaster {
				raSegs = append(raSegs, raSeg{url: absURI, dur: segDur, enc: encrypted})
			}
			rewritten = append(rewritten, core.AppendProxySig(fmt.Sprintf("%s/%s?data=%s&origin=%s&cookies=%s%s%s%s%s",
				proxyBase, endpoint, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix, encSuffix)))
		}
	}

	// VOD only (a live window slides on its own), and not byte ranges of
	// shared files (those go through Range requests, outside the cache).
	if len(raSegs) > 0 && bytes.Contains(content, []byte("#EXT-X-ENDLIST")) && !bytes.Contains(content, []byte("#EXT-X-BYTERANGE")) {
		readAheadRegister(&raList{
			segs: raSegs, origin: realOrigin, cookiesRaw: cookiesRaw, xhdrRaw: xhdrRaw,
			useVPN: useVPN, egr: egr,
		})
	} else if liveFetched && len(raSegs) > 0 && !bytes.Contains(content, []byte("#EXT-X-BYTERANGE")) {
		// Live: pull the newest segments into RAM for every player (proxy_live.go).
		liveBufferTail(&raList{
			segs: raSegs, origin: realOrigin, cookiesRaw: cookiesRaw, xhdrRaw: xhdrRaw,
			useVPN: useVPN, egr: egr, ttl: liveSegTTL,
		})
	}

	if isMaster && len(extSubs) > 0 {
		rewritten = injectExtSubs(rewritten, extSubs, proxyBase)
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

// fixVariantResolution corrects an #EXT-X-STREAM-INF line's RESOLUTION when
// the variant it introduces (the next URI line in rest) was measured larger
// than declared (hls_resolution.go): that is what makes a decoder sized on
// the declared value fail. A smaller real picture (a letterboxed film coded
// as 1920x800 under "1920x1080") is left as declared.
func fixVariantResolution(line string, rest []string, playlistBase string) string {
	for _, next := range rest {
		next = strings.TrimSpace(next)
		if next == "" || strings.HasPrefix(next, "#") {
			continue
		}
		variantURL := proxyURLJoin(playlistBase, next)
		w, h, ok := lookupVariantDims(variantURL)
		if !ok {
			return line
		}
		if m := reResolution.FindStringSubmatch(line); m != nil {
			dw, _ := strconv.Atoi(m[1])
			dh, _ := strconv.Atoi(m[2])
			if w <= dw && h <= dh {
				return line
			}
		}
		fixed, declared, changed := correctStreamInfResolution(line, w, h)
		if changed {
			log.Printf("[proxy/playlist] variant declares RESOLUTION %q, its stream is %dx%d — corrected url=%s", declared, w, h, logURL(variantURL))
		}
		return fixed
	}
	return line
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
	// A byte range from the player (EXT-X-BYTERANGE, a seek in a progressive
	// file) is forwarded upstream and relayed as it comes: no warm cache, no
	// shared fetch, no normalisation (all three work on whole segments).
	playerRange := playerByteRange(r.Header.Get("Range"))

	sess, ok := claimProxySession(w, uidRaw)
	if !ok {
		return
	}
	if playerRange == "" {
		// Fetch what follows into the warm cache (proxy_readahead.go).
		readAheadFrom(realURL)
	}
	if q.Get("nr") == "1" {
		// Not a main-rendition segment (alternate audio, init section, part):
		// it must not move the session's position estimate.
		sess = nil
	}

	// Warm-cache hit: the ResolveStream pre-buffer, or a shared fetch whose
	// first player had gone (proxy_segment_flight.go).
	serveCached := func(body []byte, how string) {
		ct := mediaTypeAtStart(body)
		if ct == "" || encrypted {
			ct = segmentTypeFromURL(realURL)
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if proxyDebug || how != "warm-cache" {
			log.Printf("[proxy/segment] %s hit (%d bytes) after %s url=%s", how, len(body), time.Since(start).Round(time.Millisecond), logURL(realURL))
		}
		// Only a delivered segment counts toward the position estimate.
		if _, err := w.Write(body); err != nil && r.Context().Err() != nil {
			log.Printf("[proxy/segment] client disconnected on %s hit after %s url=%s", how, time.Since(start).Round(time.Millisecond), logURL(realURL))
		} else if sess != nil {
			sess.RecordFetch()
		}
	}
	var flight *segFlight // set when this request leads a shared fetch
	if playerRange == "" {
		if body, ok := managers.Segments.Get(realURL); ok {
			serveCached(body, "warm-cache")
			return
		}

		// Join a fetch of the same segment already under way (typically this
		// player's own earlier request, which it gave up on).
		f, leader := joinSegFlight(realURL)
		if leader {
			flight = f
			defer flight.finish(realURL)
		} else {
			select {
			case <-f.done:
			case <-f.ready:
				// The leader is streaming it: read along from byte 0.
				if serveJoined(w, r, f.stream, start, realURL, sess) {
					f.waiters.Add(-1)
					return
				}
				// Couldn't join (its start is gone, or it failed): wait for the end.
				select {
				case <-f.done:
				case <-r.Context().Done():
					f.waiters.Add(-1)
					return
				}
			case <-r.Context().Done():
				f.waiters.Add(-1)
				return
			}
			f.waiters.Add(-1)
			if body, ok := managers.Segments.Get(realURL); ok {
				serveCached(body, "shared-fetch")
				return
			}
			// The leader failed or kept it: fetch on our own.
		}
	}
	// abandon: the player is gone and nobody waits for these bytes.
	abandon := func() bool {
		return r.Context().Err() != nil && (flight == nil || flight.waiters.Load() == 0)
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
	// bounds the whole loop (see segmentRetryBudget), not each attempt. A
	// leader's fetch outlives its player's connection (see abandon).
	parentCtx := r.Context()
	if flight != nil {
		parentCtx = context.WithoutCancel(r.Context())
	}
	budgetCtx, cancel := context.WithTimeout(parentCtx, segmentRetryBudget)
	defer cancel()

	const maxAttempts = 4
	var resp *http.Response
	var fullBody []byte // the whole segment, when it fit in segmentBufferLimit
	// Set when the body's head shows clean media: relayed as it arrives (see
	// proxy_segment_stream.go).
	var streamHead []byte
	var streamCT string
	ranged := false // relaying the upstream answer to playerRange
	// stalled: the previous attempt got no answer or was cut — retry on a new
	// connection, the pooled one may be what is stuck.
	stalled := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// A fresh request per attempt (net/http doesn't allow reusing one).
		req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, q.Get("cookies"), q.Get("xhdr"))
		if err != nil {
			// The error carries the upstream URL, token included: log it trimmed.
			log.Printf("[proxy/segment] bad upstream request url=%s", logURL(realURL))
			http.Error(w, "bad upstream url", http.StatusBadGateway)
			return
		}
		if stalled {
			req.Header.Set(freshConnHeader, "1")
		}
		if playerRange != "" {
			req.Header.Set("Range", playerRange)
		}
		// Cancelled when the retry budget is spent (or, for a follower fetching
		// on its own, when its player disconnects), or when this attempt gets
		// no response headers in time. The budget stops applying once the body
		// is streamed (stopBudget): that may last as long as bytes keep coming
		// (a long progressive file), and segStream watches for a stalled body.
		attemptCtx, attemptCancel := context.WithCancel(parentCtx)
		defer attemptCancel() // the body may stream past this iteration
		stopBudget := context.AfterFunc(budgetCtx, attemptCancel)
		ttfb := segmentAttemptTTFB(attempt)
		ttfbTimer := time.AfterFunc(ttfb, attemptCancel)
		req = req.WithContext(attemptCtx)

		var doErr error
		resp, doErr = segClient.Do(req)
		if !ttfbTimer.Stop() && budgetCtx.Err() == nil {
			// Fired: headers didn't arrive in time (any late response is
			// already cancelled).
			if doErr == nil {
				resp.Body.Close()
			}
			doErr = fmt.Errorf("no response headers within %s", ttfb)
		}
		doErr = core.RedactURLError(doErr)
		if doErr != nil {
			if abandon() {
				// The player gave up before upstream answered, and nobody waits.
				log.Printf("[proxy/segment] client disconnected after %s waiting on upstream (no response yet) url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
				return
			}
			if budgetCtx.Err() != nil {
				// Retry budget spent: every further attempt would fail at once.
				log.Printf("[proxy/segment] retry budget (%s) exhausted after %s url=%s", segmentRetryBudget, time.Since(start).Round(time.Millisecond), logURL(realURL))
				http.Error(w, "upstream timeout", http.StatusBadGateway)
				return
			}
			stalled = true
			if attempt < maxAttempts {
				log.Printf("[proxy/segment] fetch error (attempt %d/%d), retrying on a new connection url=%s: %v", attempt, maxAttempts, logURL(realURL), doErr)
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

		if playerRange != "" && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			// The player asked past the end: its answer, not an upstream failure.
			if cr := resp.Header.Get("Content-Range"); cr != "" {
				w.Header().Set("Content-Range", cr)
			}
			resp.Body.Close()
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
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
				if abandon() {
					return
				}
				continue
			}
			log.Printf("[proxy/segment] upstream %d url=%s", resp.StatusCode, logURL(realURL))
			http.Error(w, fmt.Sprintf("upstream %d", resp.StatusCode), http.StatusBadGateway)
			return
		}

		if playerRange != "" {
			if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
				log.Printf("[proxy/segment] range request answered with an HTML page — rejecting url=%s", logURL(realURL))
				noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
				resp.Body.Close()
				http.Error(w, "non-video segment", http.StatusBadGateway)
				return
			}
			stopBudget()
			ranged = true
			break
		}

		// The head tells clean media, relayed as it arrives, from a body to
		// normalise as a whole (buffered: a connection dropping halfway is still
		// retryable, and the CDN slot is released before the write to the
		// player). Bodies over segmentBufferLimit are streamed either way.
		var buf []byte
		var complete bool
		head, headComplete, rerr := readSegmentHead(resp.Body)
		switch {
		case rerr != nil:
			buf = head
		case headComplete:
			buf, complete = head, true
		default:
			if sct, ok := streamableSegment(head, resp.Header.Get("Content-Type"), realURL, encrypted); ok {
				streamHead, streamCT = head, sct
			} else {
				buf, complete, rerr = readSegmentBounded(io.MultiReader(bytes.NewReader(head), resp.Body), segmentBufferLimit)
			}
		}
		if streamHead != nil {
			stopBudget()
			break
		}
		if rerr != nil {
			stalled = true
			resp.Body.Close()
			if abandon() {
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
			fullBody = buf
			gateRelease()
		} else {
			// Too big to buffer and not streamable: copied through as it comes.
			// Like a streamed body, it is bounded by an idle watchdog instead of
			// the retry budget, and it doesn't hold the CDN slot meanwhile.
			stopBudget()
			gateRelease()
			idle := time.AfterFunc(segmentBodyIdle, attemptCancel)
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(buf), idleResetReader{resp.Body, idle}), resp.Body}
		}
		break
	}
	if streamHead != nil || ranged {
		st := &segStream{
			w: w, r: r, start: start, realURL: realURL, resp: resp, head: streamHead,
			ct: streamCT, encrypt: encrypted, flight: flight, sess: sess,
			release: gateRelease, ctx: parentCtx, client: segClient,
			origin: realOrigin, cookies: q.Get("cookies"), xhdr: q.Get("xhdr"),
			resumeOK: !resp.Uncompressed, rangeEnd: -1,
		}
		if ranged {
			st.setRanged(resp)
		}
		st.run()
		return
	}
	defer resp.Body.Close()

	// Buffered segment (its head needed more than a pass-through): normalise
	// it as a whole — see segment_normalize.go.
	if fullBody != nil {
		v := normalizeSegment(fullBody, resp.Header.Get("Content-Type"), realURL, encrypted)
		if v.reject != "" {
			preview := ""
			if os.Getenv("MYCELIUM_DEBUG") == "1" {
				preview = " body[:400]=" + strconv.Quote(string(fullBody[:min(len(fullBody), 400)]))
			}
			log.Printf("[proxy/segment] %s (content-type=%q, %d bytes) — rejecting url=%s%s", v.reject, resp.Header.Get("Content-Type"), len(fullBody), logURL(realURL), preview)
			// Challenges are often served as 200 with an HTML body: record it here.
			noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
			http.Error(w, "non-video segment", http.StatusBadGateway)
			return
		}
		for _, n := range v.notes {
			log.Printf("[proxy/segment] %s url=%s", n, logURL(realURL))
		}
		kept := false
		if flight != nil && (r.Context().Err() != nil || flight.waiters.Load() > 0) {
			// Someone will ask for these bytes again: keep them for the retry.
			managers.Segments.Put(realURL, v.body)
			kept = true
			if r.Context().Err() != nil {
				log.Printf("[proxy/segment] player gone after %s, segment kept for its retry url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
				return
			}
		}
		if v.ct == "" {
			v.ct = segmentTypeFromURL(realURL)
		}
		w.Header().Set("Content-Type", v.ct)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Length", strconv.Itoa(len(v.body)))
		if proxyDebug {
			log.Printf("[proxy/segment] ok url=%s", logURL(realURL))
		}
		if _, err := w.Write(v.body); err != nil && r.Context().Err() != nil {
			if flight != nil && !kept {
				// Gone while being served: its retry finds the bytes here.
				managers.Segments.Put(realURL, v.body)
			}
			log.Printf("[proxy/segment] player closed the request after %s (seek, quality switch or retry) url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
		} else if sess != nil {
			sess.RecordFetch()
		}
		return
	}

	// Streamed segment (over segmentBufferLimit): decide from its head.
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
		return
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
		log.Printf("[proxy/key] bad upstream request url=%s", logURL(realURL))
		http.Error(w, "bad upstream url", http.StatusBadGateway)
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

// idleResetReader re-arms t on every read, so t fires only when the body
// stalls for its whole duration.
type idleResetReader struct {
	r io.Reader
	t *time.Timer
}

func (ir idleResetReader) Read(b []byte) (int, error) {
	n, err := ir.r.Read(b)
	ir.t.Reset(segmentBodyIdle)
	return n, err
}
