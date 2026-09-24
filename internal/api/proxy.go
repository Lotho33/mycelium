package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// proxyDebug gates the verbose proxy log lines that dump full request/response
// headers and whole playlist bodies — those carry upstream Cookie/Authorization
// and tokenised segment URLs and must not land in data/logs/*.log by default.
var proxyDebug = os.Getenv("MYCELIUM_DEBUG") == "1"

// proxyAllowUnsigned disables the /proxy/* URL signature check. Escape hatch for
// debugging only — with it set the HLS proxy is an open relay again.
var proxyAllowUnsigned = os.Getenv("MYCELIUM_PROXY_ALLOW_UNSIGNED") == "1"

// segmentRetryBudget is the overall time budget for ProxySegment's whole
// retry loop (all maxAttempts attempts combined), not per attempt. Each
// attempt is already capped by the segment client's own Timeout (60s,
// getSegmentClient/vpnClients), but that's per-attempt: a CDN that's merely
// slow on every attempt — never failing outright, just crawling toward its
// own 60s timeout each time — could otherwise cost up to maxAttempts×60s
// plus backoff (worst case ~4-4.5 minutes) before the handler gives up,
// which the user experiences as a frozen player.
//
// 45s is chosen to comfortably fit one legitimate fetch plus a couple of
// quick retries (a real segment fetch, even over a slow CDN, normally
// completes in a few seconds) while staying well under the previous
// multi-minute worst case — long enough not to abort streams that are
// merely a bit slow, short enough that a stuck segment fails fast instead of
// hanging the player for minutes.
//
// A var (not a const) so tests can shrink it instead of waiting out real
// time; production code never reassigns it.
var segmentRetryBudget = 45 * time.Second

// playlistReadLimit caps an upstream HLS playlist body. Real playlists are a
// few hundred KB at most, even long live windows or big VOD ladders.
const playlistReadLimit = 8 << 20 // 8 MiB

// requireProxySig rejects a /proxy/* request whose query carries no valid HMAC
// (minted by media_handler.go / the playlist rewriter). Returns true if the
// caller should stop. No-op when MYCELIUM_PROXY_ALLOW_UNSIGNED=1 or when no
// signing key is configured (keeps a keyless dev boot working).
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

// logURL trims a URL for logging: scheme://host/path only, the query replaced
// with a marker. Both the incoming /proxy/* URLs (base64'd upstream URL +
// cookies in the query) and the decoded upstream URLs (CDN auth tokens in the
// query) would otherwise leak secrets into data/logs/*.log. Full URLs are
// logged only under MYCELIUM_DEBUG=1 (proxyDebug).
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
		// absolute URL (an upstream target); a bare incoming request URL is
		// path-only and stays that way — no "://" prefix.
		s = u.Scheme + "://" + u.Host + u.Path
	}
	if u.RawQuery != "" {
		s += "?<redacted>"
	}
	return s
}

var reKeyURI = regexp.MustCompile(`URI=["']([^"']+)["']`)

func proxyResolveForPlaylist(r *http.Request, sidRaw, uidRaw string) (newPlaylistURL, newOrigin string) {
	decoded := proxyB64Decode(sidRaw)
	if decoded == "" {
		return "", ""
	}
	idx := strings.IndexByte(decoded, 0)
	if idx < 0 {
		return "", ""
	}
	pluginID := decoded[:idx]
	sourceID := decoded[idx+1:]

	var resolvedURL string
	var resolvedHeaders map[string]string

	if engine.LuaPlugins.Has(pluginID) {
		// A stopped plugin must not run Lua, even for a player still holding
		// one of its playlist URLs.
		if !engine.LuaPlugins.IsOperational(pluginID) {
			return "", ""
		}
		// Lua plugins live in a separate registry from the native/gRPC ones
		// resolve through the Lua entrypoint directly,
		// same call lua_pipeline.go's ResolveStream makes.
		// force_refresh tells plugins that cache their resolved URL to skip the
		// cache read and resolve a fresh CDN token instead of handing back the
		// same one that just got rejected.
		b, err := engine.LuaPlugins.CallEntrypointJSON(pluginID, engine.EPResolveStream, map[string]any{
			"stream_id":     sourceID,
			"force_refresh": true,
		}, "")
		if err != nil {
			log.Printf("[proxy/playlist] re-resolve %s/%s failed: %v", pluginID, sourceID, err)
			return "", ""
		}
		var result struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(b, &result); err != nil || result.URL == "" {
			log.Printf("[proxy/playlist] re-resolve %s/%s: invalid lua result: %v", pluginID, sourceID, err)
			return "", ""
		}
		resolvedURL, resolvedHeaders = result.URL, result.Headers
	} else {
		log.Printf("[proxy/playlist] re-resolve: plugin %q not found", pluginID)
		return "", ""
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return buildResolvedPlaylistURL(scheme, r.Host, pluginID, sourceID, resolvedURL, resolvedHeaders, uidRaw)
}

// buildResolvedPlaylistURL mints the signed /proxy/playlist.m3u8 redirect
// target for a re-resolved stream. Split out from proxyResolveForPlaylist so
// the URL-building + signing is unit-testable without a real Lua plugin.
//
// This is the one mint site the P1-2 HMAC pass (core.AppendProxySig) first
// missed: without it, the 302 ProxyPlaylist issues to this URL sent the
// player to an unsigned target that requireProxySig then rejected with 403 —
// the whole "CDN token expired, re-resolve and retry" path was broken.
func buildResolvedPlaylistURL(scheme, host, pluginID, sourceID, resolvedURL string, resolvedHeaders map[string]string, uidRaw string) (newURL, origin string) {
	// cobweb lowercases sniffed header names; the lookups below and buildXhdrSuffix
	// are title-cased. Canonicalise so a captured Cookie survives the re-resolve.
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
	// rr=1 marca questo URL come già ri-risolto: se anche il fetch successivo
	// fallisce, ProxyPlaylist restituisce 502 invece di ri-risolvere di nuovo,
	// evitando un loop di redirect 302 (token nuovo, stesso 403).
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

// ProxyPlaylist godoc
//
//	@Summary		Proxy playlist HLS
//	@Description	Scarica la playlist M3U8 upstream e riscrive i segmenti/chiavi per passare dal proxy locale. Senza autenticazione — chiamato direttamente dai media player.
//	@Tags			Proxy HLS
//	@Param			data	query	string	true	"URL playlist upstream codificato in base64url"
//	@Param			origin	query	string	false	"Origin upstream codificato in base64url"
//	@Param			cookies	query	string	false	"Cookie upstream codificati in base64url"
//	@Produce		application/vnd.apple.mpegurl
//	@Success		200	{string}	string	"playlist M3U8 riscritta"
//	@Failure		502	{string}	string	"upstream error"
//	@Router			/proxy/playlist.m3u8 [get]
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
	uidRaw := q.Get("uid") // user session identifier for tracking
	useVPN := q.Get("vpn") == "1"
	egr := q.Get("egr") // egress profile name (see videoEgressSuffix)

	realURL := proxyB64Decode(dataRaw)
	realOrigin := proxyB64Decode(originRaw)

	log.Printf("[proxy/playlist] url=%s origin=%s uid=%s xhdr_present=%v vpn=%v egr=%s", logURL(realURL), realOrigin, uidRaw, xhdrRaw != "", useVPN, egr)

	plClient, clErr := upstreamPlaylistClient(useVPN, egr)
	if clErr != nil {
		log.Printf("[proxy/playlist] %v url=%s", clErr, logURL(realURL))
		http.Error(w, clErr.Error(), http.StatusBadGateway)
		return
	}

	// Warm-cache hit: the pre-buffer step (prebuffer.go) already fetched this
	// exact upstream playlist during ResolveStream. Serve those bytes and skip
	// the round-trip — the rewrite below still runs, so a master playlist is
	// still rewritten to proxy variant URLs.
	var content []byte
	var fetchErr error
	if warm, ok := managers.Segments.Get(realURL); ok {
		content = warm
		// Serve the resolve-time copy once (fast first frame), then drop it:
		// a live media playlist's segment window slides, so mpv's next
		// reload must fetch a fresh copy from upstream. Keeping the frozen
		// copy for the whole 90s TTL is why live streams "stop transmitting
		// after a while" — mpv replays the same few segments and then runs
		// dry with no #EXT-X-ENDLIST to tell it the stream really ended.
		managers.Segments.Delete(realURL)
		log.Printf("[proxy/playlist] warm-cache hit (%d bytes), evicted for reload url=%s", len(warm), logURL(realURL))
	} else {
		// Deduplicate concurrent upstream fetches for the same URL.
		// All callers share one HTTP request; useful for live streams with multiple viewers.
		var shared bool
		content, fetchErr, shared = playlistInflight.Do(realURL, func() ([]byte, error) {
			req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, cookiesRaw, xhdrRaw)
			if err != nil {
				return nil, err
			}
			if proxyDebug {
				log.Printf("[proxy/playlist] sending headers=%v", req.Header)
			}
			// Detached from r.Context() on purpose: playlistInflight shares
			// this one upstream fetch across every concurrent caller, and
			// mpv opens a fresh connection for every HLS playlist reload
			// then closes it as soon as it has the body. If this fetch rode
			// the leader request's context, that leader closing its
			// connection (or any single client going away) would cancel the
			// fetch the other callers — and mpv's very next reload — are
			// blocked on, which surfaced as "Failed to reload playlist 0" /
			// "parse_playlist error Invalid data found" a few seconds into
			// every live stream.
			//
			// A live origin can answer a playlist reload with a transient
			// 404/5xx (encoder restart, on-demand pull warming up). One such
			// blip used to end playback for good, so retry those briefly;
			// 401/403 (likely an expired token) fall straight through to the
			// re-resolve path instead.
			const maxPlaylistAttempts = 3
			for attempt := 1; ; attempt++ {
				req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, cookiesRaw, xhdrRaw)
				if err != nil {
					return nil, err
				}
				if proxyDebug {
					log.Printf("[proxy/playlist] sending headers=%v", req.Header)
				}
				fetchCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				req = req.WithContext(fetchCtx)
				resp, err := plClient.Do(req)
				if err != nil {
					cancel()
					return nil, err
				}
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					// Bounded: a hostile/broken upstream answering with a
					// multi-GB "playlist" would otherwise OOM the container.
					b, rerr := io.ReadAll(io.LimitReader(resp.Body, playlistReadLimit+1))
					resp.Body.Close()
					cancel()
					if rerr == nil && len(b) > playlistReadLimit {
						return nil, fmt.Errorf("playlist upstream oltre %d MiB", playlistReadLimit>>20)
					}
					return b, rerr
				}
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
				bodyStr := strings.TrimSpace(string(body))
				if len(bodyStr) > 200 {
					bodyStr = bodyStr[:200]
				}
				log.Printf("[proxy/playlist] upstream %d body=%q", resp.StatusCode, bodyStr)
				if proxyDebug {
					log.Printf("[proxy/playlist] upstream %d headers=%v", resp.StatusCode, resp.Header)
				}
				noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
				status := resp.StatusCode
				resp.Body.Close()
				cancel()
				transient := status == http.StatusNotFound || status >= 500
				if !transient || attempt >= maxPlaylistAttempts {
					return nil, fmt.Errorf("upstream %d", status)
				}
				time.Sleep(time.Duration(attempt) * 700 * time.Millisecond)
			}
		})
		if shared {
			log.Printf("[proxy/playlist] shared fetch for %s", logURL(realURL))
		}
	}

	if fetchErr == nil && content != nil {
		segs := strings.Count(string(content), "#EXTINF")
		if proxyDebug {
			log.Printf("[proxy/playlist] fetched %d bytes, %d segments:\n%s", len(content), segs, string(content))
		} else {
			log.Printf("[proxy/playlist] fetched %d bytes, %d segments", len(content), segs)
		}
	}

	if fetchErr != nil {
		log.Printf("[proxy/playlist] fetch error: %v", fetchErr)
		if sidRaw != "" && q.Get("rr") == "" {
			if newURL, _ := proxyResolveForPlaylist(r, sidRaw, uidRaw); newURL != "" {
				log.Printf("[proxy/playlist] re-resolve → %s", newURL)
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

	var rewritten []string
	var lastExtInf float64
	// A media playlist with #EXT-X-KEY declares its segments encrypted: their
	// bytes are ciphertext (no TS/fMP4 signature, whatever the URL extension or
	// Content-Type says — some CDNs serve them as .html/.webp/.jpg). Tell
	// ProxySegment so it relays them verbatim instead of "sniffing" random
	// bytes and rejecting them as a non-video page.
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
					if strings.HasPrefix(line, "#EXT-X-KEY") {
						replacement = fmt.Sprintf("%s/key.key?data=%s&origin=%s&cookies=%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					} else if strings.HasPrefix(line, "#EXT-X-MAP") {
						// fMP4 init segment: binary resource, must go through segment proxy.
						// Routing it through playlist.m3u8 would return wrong Content-Type
						// and break playback silently (player never requests segments).
						replacement = fmt.Sprintf("%s/segment.ts?data=%s&origin=%s&cookies=%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					} else {
						replacement = fmt.Sprintf("%s/playlist.m3u8?data=%s&origin=%s&cookies=%s%s%s%s", proxyBase, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix)
					}
					line = strings.Replace(line, m[1], core.AppendProxySig(replacement), 1)
				}
			}
			rewritten = append(rewritten, line)
		} else {
			// Segment URL — update session segment duration from preceding #EXTINF.
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
			rewritten = append(rewritten, core.AppendProxySig(fmt.Sprintf("%s/%s?data=%s&origin=%s&cookies=%s%s%s%s%s",
				proxyBase, endpoint, encURI, originRaw, cookiesRaw, uidSuffix, xhdrSuffix, vpnSuffix, encSuffix)))
		}
	}

	out := strings.Join(rewritten, "\n")
	if proxyDebug {
		log.Printf("[proxy/playlist] rewritten (%d lines):\n%s", len(rewritten), out)
	} else {
		log.Printf("[proxy/playlist] rewritten (%d lines)", len(rewritten))
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprint(w, out)
}

// ProxySegment godoc
//
//	@Summary		Proxy segmento TS
//	@Description	Scarica e streama un segmento video TS direttamente senza buffering. Senza autenticazione.
//	@Tags			Proxy HLS
//	@Param			data	query	string	true	"URL segmento upstream codificato in base64url"
//	@Param			origin	query	string	false	"Origin upstream codificato in base64url"
//	@Param			cookies	query	string	false	"Cookie upstream codificati in base64url"
//	@Produce		video/MP2T
//	@Success		200	{string}	string	"stream binario segmento"
//	@Failure		502	{string}	string	"upstream error"
//	@Router			/proxy/segment.ts [get]
func ProxySegment(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	log.Printf("[proxy/segment] INCOMING from=%s url=%s", r.RemoteAddr, logURL(r.URL.String()))
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

	// Warm-cache hit: the pre-buffer step (prebuffer.go) already fetched and
	// normalized this exact segment during ResolveStream. Serve it from RAM —
	// this is what turns "3/8 segmenti" progress into a near-instant start.
	if body, ok := managers.Segments.Get(realURL); ok {
		ct := "video/MP2T"
		if lu := strings.ToLower(realURL); strings.Contains(lu, ".mp4") || strings.Contains(lu, ".m4s") || strings.Contains(lu, ".cmfv") || strings.Contains(lu, ".cmfa") {
			ct = "video/mp4"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		log.Printf("[proxy/segment] warm-cache hit (%d bytes) url=%s", len(body), logURL(realURL))
		if _, err := w.Write(body); err != nil && r.Context().Err() != nil {
			log.Printf("[proxy/segment] client disconnected on warm hit after %s url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
		}
		if uidRaw != "" {
			if s, ok := managers.Sessions.Get(uidRaw); ok {
				s.RecordFetch()
			}
		}
		return
	}

	segClient, clErr := upstreamSegmentClient(useVPN, egr)
	if clErr != nil {
		log.Printf("[proxy/segment] %v url=%s", clErr, logURL(realURL))
		http.Error(w, clErr.Error(), http.StatusBadGateway)
		return
	}

	// Cap concurrent upstream fetches per CDN host so a player's start-of-stream
	// burst can't trip a rate-limiting segment CDN. Released as soon as we're
	// done reading the upstream response.
	var segHost string
	if u, perr := url.Parse(realURL); perr == nil {
		segHost = u.Host
	}
	gateRelease, gerr := segAcquire(r.Context(), segHost)
	if gerr != nil {
		log.Printf("[proxy/segment] client gone while queued url=%s", logURL(realURL))
		return
	}
	defer gateRelease()

	// Retry only up to the point where we haven't written anything to the
	// player yet (no headers, no bytes sent): once io.Copy below starts, the
	// response is committed and can't be replayed. This catches transient
	// connection failures and bad statuses (DNS blip, edge handshake
	// failure, edge briefly down, a rate-limit 403 that clears after a
	// backoff) — not a mid-stream truncation after bytes have already
	// started flowing, which is a separate failure mode.
	//
	// Each individual attempt is already bounded by the segment client's own
	// Timeout (60s, getSegmentClient/vpnClients) — but that's per-attempt,
	// not per-request: a CDN that's merely slow on every attempt (never
	// failing outright, just crawling toward its own 60s timeout each time)
	// could otherwise burn up to maxAttempts×60s plus backoff — worst case
	// ~4-4.5 minutes — before this handler finally gives up with a 502,
	// which reads to the user as a frozen player. budgetCtx wraps the whole
	// loop in one shared deadline so a single slow attempt gets cut off
	// early once the overall budget is spent, and the loop stops retrying
	// altogether past that point instead of letting every attempt separately
	// run down its own 60s allowance.
	budgetCtx, cancel := context.WithTimeout(r.Context(), segmentRetryBudget)
	defer cancel()

	const maxAttempts = 4
	var resp *http.Response
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// A fresh *http.Request per attempt: reusing one across multiple
		// Client.Do calls isn't part of the net/http contract.
		req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, q.Get("cookies"), q.Get("xhdr"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Annulla il download CDN se il client (player) chiude la connessione
		// o se il budget complessivo del retry loop è scaduto. budgetCtx
		// deriva da r.Context(), quindi la disconnessione del client lo
		// annulla comunque — nessuna divergenza tra i due Done().
		req = req.WithContext(budgetCtx)

		var doErr error
		resp, doErr = segClient.Do(req)
		if doErr != nil {
			if r.Context().Err() != nil {
				// Client disconnesso prima che l'upstream rispondesse: se il player ha
				// mollato dopo pochi secondi, è quasi certamente perché l'host CDN di
				// questo segmento era lento/irraggiungibile — vale la pena tracciarlo,
				// a differenza di una disconnessione tardiva (utente ha chiuso il player).
				log.Printf("[proxy/segment] client disconnected after %s waiting on upstream (no response yet) url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
				return
			}
			if budgetCtx.Err() != nil {
				// Client ancora connesso ma il budget complessivo del retry
				// loop è scaduto (CDN lento su ogni tentativo, mai un fallimento
				// netto): niente altri tentativi, ognuno fallirebbe comunque
				// all'istante con lo stesso context già scaduto.
				log.Printf("[proxy/segment] retry budget (%s) exhausted after %s url=%s", segmentRetryBudget, time.Since(start).Round(time.Millisecond), logURL(realURL))
				http.Error(w, "upstream timeout", http.StatusBadGateway)
				return
			}
			if attempt < maxAttempts {
				log.Printf("[proxy/segment] fetch error (attempt %d/%d) url=%s: %v", attempt, maxAttempts, logURL(realURL), doErr)
				time.Sleep(retryBackoff(attempt))
				continue
			}
			log.Printf("[proxy/segment] fetch error url=%s: %v", logURL(realURL), doErr)
			http.Error(w, doErr.Error(), http.StatusBadGateway)
			return
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Same challenge signal ProxyPlaylist records on a non-2xx status
			// (see there) — segments can be individually challenged even when
			// the playlist itself wasn't, and until now that never surfaced
			// here, so an operator only ever saw it in raw logs, never in the
			// dashboard's pending-challenges list.
			noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
			resp.Body.Close()
			if attempt < maxAttempts {
				d := segRetryBackoff(attempt, resp.StatusCode)
				log.Printf("[proxy/segment] upstream %d (attempt %d/%d, wait %s) url=%s", resp.StatusCode, attempt, maxAttempts, d, logURL(realURL))
				select {
				case <-time.After(d):
				case <-budgetCtx.Done():
					// Copre sia la disconnessione del client (budgetCtx deriva
					// da r.Context()) sia lo scadere del budget complessivo:
					// in entrambi i casi non ha senso attendere il resto del
					// backoff per poi ritentare comunque.
					return
				}
				continue
			}
			log.Printf("[proxy/segment] upstream %d url=%s", resp.StatusCode, logURL(realURL))
			http.Error(w, fmt.Sprintf("upstream %d", resp.StatusCode), http.StatusBadGateway)
			return
		}
		break
	}
	defer resp.Body.Close()

	// Pass through the CDN's Content-Type so fMP4 segments (.mp4/.m4s) are
	// served as video/mp4, not video/MP2T. Players reject init segments and
	// media segments with the wrong MIME type.
	ct := resp.Header.Get("Content-Type")
	bodyModified := false
	switch {
	case encrypted:
		// Ciphertext (HLS AES-128): nothing to sniff, and the wire
		// Content-Type is camouflage (text/html, image/webp, ...). Relay the
		// bytes untouched for the player to decrypt — unless the body is
		// plainly a text page, which is a CDN error/challenge, not a segment.
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
		// correct type — pass through as-is
	case strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "binary/"):
		// Some CDNs wrap the real segment in a fake image container
		// (content-type image/png|jpg|webp, real image bytes up front). Browser
		// players like hls.js scan the whole payload for TS sync bytes; strict
		// demuxers like ffmpeg/libmpv expect sync at offset 0 and fail at once.
		//
		// Find where the media actually starts (TS or fMP4) and serve from
		// there. The wrapper can be a full image, so the search window is
		// mediaSniffLimit, not a few KB. If nothing is found, pass the raw
		// bytes anyway as a fallback.
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
		// text/html, text/javascript, application/octet-stream mislabelled,
		// etc. Some CDNs deliberately serve media segments under a non-media
		// content-type (or host them on a generic file host that labels by
		// extension). Sniff the payload for an actual media signature before
		// deciding: relay it as video if it really contains one, reject it as
		// a genuine error page only if it doesn't.
		sniff, off, mediaCT, serr := sniffMediaStart(resp.Body)
		if serr != nil {
			log.Printf("[proxy/segment] content-type=%q read error: %v url=%s", ct, serr, logURL(realURL))
			http.Error(w, "segment read error", http.StatusBadGateway)
			return
		}
		if off < 0 {
			preview := ""
			if os.Getenv("MYCELIUM_DEBUG") == "1" {
				n := len(sniff)
				if n > 400 {
					n = 400
				}
				preview = " body[:400]=" + strconv.Quote(string(sniff[:n]))
			}
			log.Printf("[proxy/segment] content-type=%q no media signature in %d bytes — rejecting url=%s%s", ct, len(sniff), logURL(realURL), preview)
			// This is the more important of the two call sites: a CDN
			// challenge is very often served as plain 200 OK with an
			// html/js body (Cf-Mitigated set regardless of status), so it
			// never trips the non-2xx branch above — this is the only place
			// a segment-level challenge like that gets recorded at all.
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
	log.Printf("[proxy/segment] ok url=%s", logURL(realURL))
	if _, err := io.Copy(w, resp.Body); err != nil {
		if r.Context().Err() != nil {
			log.Printf("[proxy/segment] client disconnected mid-stream after %s url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
		} else {
			log.Printf("[proxy/segment] stream error: %v", err)
		}
	}

	// Aggiorna stima posizione nella sessione (best-effort, non bloccante).
	if uidRaw != "" {
		if s, ok := managers.Sessions.Get(uidRaw); ok {
			s.RecordFetch()
		}
	}
}

// ProxyKey godoc
//
//	@Summary		Proxy chiave HLS
//	@Description	Scarica e restituisce la chiave AES-128 per la decifratura dei segmenti HLS. Senza autenticazione.
//	@Tags			Proxy HLS
//	@Param			data	query	string	true	"URL chiave upstream codificato in base64url"
//	@Param			origin	query	string	false	"Origin upstream codificato in base64url"
//	@Param			cookies	query	string	false	"Cookie upstream codificati in base64url"
//	@Produce		application/octet-stream
//	@Success		200	{string}	string	"chiave binaria"
//	@Failure		502	{string}	string	"upstream error"
//	@Router			/proxy/key.key [get]
func ProxyKey(w http.ResponseWriter, r *http.Request) {
	if requireProxySig(w, r) {
		return
	}
	q := r.URL.Query()
	realURL := proxyB64Decode(q.Get("data"))
	realOrigin := proxyB64Decode(q.Get("origin"))
	useVPN := q.Get("vpn") == "1"
	egr := q.Get("egr")

	req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, q.Get("cookies"), q.Get("xhdr"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Annulla il fetch della chiave se il client (player) chiude la connessione,
	// come già fa ProxySegment — senza questo il fetch prosegue comunque fino
	// al timeout del client (15s) anche a player disconnesso.
	req = req.WithContext(r.Context())

	keyClient, clErr := upstreamPlaylistClient(useVPN, egr)
	if clErr != nil {
		log.Printf("[proxy/key] %v url=%s", clErr, logURL(realURL))
		http.Error(w, clErr.Error(), http.StatusBadGateway)
		return
	}
	resp, err := keyClient.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			log.Printf("[proxy/key] fetch error url=%s: %v", logURL(realURL), err)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Same challenge signal as ProxyPlaylist/ProxySegment (see either) —
		// the key fetch is a third, independent CDN request that can be
		// challenged on its own.
		noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
		log.Printf("[proxy/key] upstream %d url=%s", resp.StatusCode, logURL(realURL))
		http.Error(w, fmt.Sprintf("upstream %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	// An HLS key is 16 bytes (AES-128) — cap generously anyway so a
	// misbehaving/malicious upstream streaming an unbounded body can't be used
	// to exhaust memory through this route.
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
