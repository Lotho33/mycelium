package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
	"mycelium/internal/pileus"
)

// b64url is the unpadded base64url encoding every /proxy query param uses.
func b64url(v string) string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(v))
}

// ─── playback lease on /proxy ────────────────────────────────────────────────

// claimProxySession looks up the playback session behind a /proxy request's
// uid and renews its lease. It answers 409 and returns ok=false when the
// profile is now playing on another device. An unknown uid is let through.
func claimProxySession(w http.ResponseWriter, uidRaw string) (s *managers.StreamSession, ok bool) {
	if uidRaw == "" {
		return nil, true
	}
	s, found := managers.Sessions.Get(uidRaw)
	if !found {
		return nil, true
	}
	if holder, ok := s.ClaimPlayback(); !ok {
		log.Printf("[proxy] uid=%s: profile now playing on device %s — refusing", uidRaw, holder)
		http.Error(w, "riproduzione spostata su "+pileus.DeviceDisplayName(holder), http.StatusConflict)
		return s, false
	}
	return s, true
}

// ─── upstream playlist fetch ─────────────────────────────────────────────────

// playlistFetchBudget bounds one upstream playlist fetch, retries included;
// playlistAttemptTimeout bounds each attempt. Vars so tests can shrink them.
var (
	playlistFetchBudget    = 30 * time.Second
	playlistAttemptTimeout = 20 * time.Second
)

// fetchUpstreamPlaylist GETs an upstream HLS playlist. Transient 404/5xx
// (a live origin warming up) and connection errors are retried briefly;
// 401/403 fail at once so the caller can re-resolve.
//
// Detached from the client's context: playlistInflight shares this fetch
// among concurrent callers, and players drop the connection right after
// each reload.
func fetchUpstreamPlaylist(client *http.Client, realURL, realOrigin, cookiesRaw, xhdrRaw string, useVPN bool, egr string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), playlistFetchBudget)
	defer cancel()
	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		req, err := buildProxyRequest(http.MethodGet, realURL, realOrigin, cookiesRaw, xhdrRaw)
		if err != nil {
			return nil, err
		}
		if proxyDebug {
			log.Printf("[proxy/playlist] sending headers=%v", req.Header)
		}
		body, retryErr, final := playlistAttempt(ctx, client, req, realURL, useVPN, egr)
		if final {
			return body, retryErr
		}
		if attempt >= maxAttempts || ctx.Err() != nil {
			return nil, retryErr
		}
		log.Printf("[proxy/playlist] attempt %d/%d failed (%v), retrying url=%s", attempt, maxAttempts, retryErr, logURL(realURL))
		select {
		case <-time.After(time.Duration(attempt) * 700 * time.Millisecond):
		case <-ctx.Done():
			return nil, retryErr
		}
	}
}

// playlistAttempt runs one fetch. final=true means don't retry (success, or a
// non-transient failure); otherwise err is the transient failure.
func playlistAttempt(ctx context.Context, client *http.Client, req *http.Request, realURL string, useVPN bool, egr string) (body []byte, err error, final bool) {
	attemptCtx, cancel := context.WithTimeout(ctx, playlistAttemptTimeout)
	defer cancel()
	resp, err := client.Do(req.WithContext(attemptCtx))
	if err != nil {
		return nil, core.RedactURLError(err), false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Bounded against an oversized "playlist".
		b, rerr := io.ReadAll(io.LimitReader(resp.Body, playlistReadLimit+1))
		if rerr != nil {
			// Connection dropped mid-body: transient.
			return nil, rerr, false
		}
		if len(b) > playlistReadLimit {
			return nil, fmt.Errorf("playlist upstream oltre %d MiB", playlistReadLimit>>20), true
		}
		return b, nil, true
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	bodyStr := strings.TrimSpace(string(head))
	if len(bodyStr) > 200 {
		bodyStr = bodyStr[:200]
	}
	log.Printf("[proxy/playlist] upstream %d body=%q", resp.StatusCode, bodyStr)
	if proxyDebug {
		log.Printf("[proxy/playlist] upstream %d headers=%v", resp.StatusCode, resp.Header)
	}
	noteChallengeIfAny(resp, realURL, challengeEgressLabel(useVPN, egr))
	transient := resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500
	return nil, fmt.Errorf("upstream %d", resp.StatusCode), !transient
}

// ─── master playlist children ────────────────────────────────────────────────

// masterChildURIs lists a master playlist's child playlists in document
// order, resolved against base: the URI= of tags routed to the playlist
// endpoint and every URI line. ProxyPlaylist numbers the children (vi) in
// the same order, so vi picks the same rendition from a re-resolved master.
func masterChildURIs(content, base string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if uriTagTarget(line) != "playlist" {
				continue
			}
			if m := reKeyURI.FindStringSubmatch(line); len(m) == 2 {
				out = append(out, proxyURLJoin(base, m[1]))
			}
			continue
		}
		out = append(out, proxyURLJoin(base, line))
	}
	return out
}

// ─── mid-stream recovery of a child playlist ─────────────────────────────────
//
// A live player keeps reloading the variant playlist, never the master.
// Children of a master carry sid + vi: when one fails, ProxyPlaylist
// re-resolves, takes child vi from the new master and keeps serving under
// the player's unchanged URL. The mapping is remembered (playlistRemaps)
// for later reloads.

// playlistRemap is where requests for an original child URL go now.
type playlistRemap struct {
	url        string
	origin     string
	cookiesRaw string // b64url, as the proxy query carries it
	xhdrRaw    string // b64url(json)
	lastUsed   time.Time
}

const playlistRemapIdle = 2 * time.Hour

var (
	playlistRemapsMu sync.Mutex
	playlistRemaps   = map[string]*playlistRemap{}
)

func lookupPlaylistRemap(origURL string) (playlistRemap, bool) {
	playlistRemapsMu.Lock()
	defer playlistRemapsMu.Unlock()
	rm := playlistRemaps[origURL]
	if rm == nil {
		return playlistRemap{}, false
	}
	if time.Since(rm.lastUsed) > playlistRemapIdle {
		delete(playlistRemaps, origURL)
		return playlistRemap{}, false
	}
	rm.lastUsed = time.Now()
	return *rm, true
}

func storePlaylistRemap(origURL string, rm playlistRemap) {
	playlistRemapsMu.Lock()
	defer playlistRemapsMu.Unlock()
	now := time.Now()
	for k, v := range playlistRemaps {
		if now.Sub(v.lastUsed) > playlistRemapIdle {
			delete(playlistRemaps, k)
		}
	}
	rm.lastUsed = now
	playlistRemaps[origURL] = &rm
}

// reResolveCooldown: every child of a stream fails at once when a token
// expires; they share one resolve_stream call, at most once per cooldown.
const reResolveCooldown = 30 * time.Second

type reResolveEntry struct {
	mu      sync.Mutex
	at      time.Time
	url     string
	headers map[string]string
	err     error
}

var (
	reResolveMu    sync.Mutex
	reResolveCache = map[string]*reResolveEntry{}
)

// sessionProfile is the profile behind a /proxy uid — the playback session
// ResolveStream registered, or a download's owner (managers.ProxyOwners) —
// "" when unknown (URLs minted before a restart).
func sessionProfile(uidRaw string) string {
	if uidRaw == "" {
		return ""
	}
	if s, ok := managers.Sessions.Get(uidRaw); ok {
		return s.ProfileID
	}
	if pid, ok := managers.ProxyOwners.Profile(uidRaw); ok {
		return pid
	}
	return ""
}

// reResolveShared is reResolveLua coalesced per sid and profile: concurrent
// callers wait for one call, and its result is reused for
// reResolveCooldown. The profile is in the key: per-profile plugin logins
// give different URLs.
func reResolveShared(sidRaw, profileID string) (string, map[string]string, error) {
	reResolveMu.Lock()
	now := time.Now()
	for k, e := range reResolveCache {
		if e.mu.TryLock() {
			stale := now.Sub(e.at) > 10*reResolveCooldown
			e.mu.Unlock()
			if stale {
				delete(reResolveCache, k)
			}
		}
	}
	key := sidRaw + "\x00" + profileID
	e := reResolveCache[key]
	if e == nil {
		e = &reResolveEntry{}
		reResolveCache[key] = e
	}
	reResolveMu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.at.IsZero() && time.Since(e.at) < reResolveCooldown {
		return e.url, e.headers, e.err
	}
	pluginID, sourceID, ok := decodeSid(sidRaw)
	if !ok {
		e.url, e.headers, e.err = "", nil, errors.New("sid non valido")
	} else {
		e.url, e.headers, e.err = reResolveLua(pluginID, sourceID, profileID)
	}
	e.at = time.Now()
	return e.url, e.headers, e.err
}

// decodeSid splits the sid param (b64url of pluginID NUL sourceID).
func decodeSid(sidRaw string) (pluginID, sourceID string, ok bool) {
	decoded := proxyB64Decode(sidRaw)
	idx := strings.IndexByte(decoded, 0)
	if idx <= 0 {
		return "", "", false
	}
	return decoded[:idx], decoded[idx+1:], true
}

// reResolveLua calls the plugin's resolve_stream again with force_refresh,
// under the playback's profile, so the plugin hands out a fresh token.
func reResolveLua(pluginID, sourceID, profileID string) (string, map[string]string, error) {
	if !engine.LuaPlugins.Has(pluginID) {
		return "", nil, fmt.Errorf("plugin %q non trovato", pluginID)
	}
	// A stopped plugin must not run Lua.
	if !engine.LuaPlugins.IsOperational(pluginID) {
		return "", nil, fmt.Errorf("plugin %q non operativo", pluginID)
	}
	b, err := engine.LuaPlugins.CallEntrypointJSON(pluginID, engine.EPResolveStream, map[string]any{
		"stream_id":     sourceID,
		"force_refresh": true,
	}, profileID)
	if err != nil {
		return "", nil, err
	}
	var result struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return "", nil, fmt.Errorf("risultato lua non valido: %w", err)
	}
	if result.URL == "" {
		return "", nil, errors.New("risultato lua senza url")
	}
	// Sniffed header names can be lowercase; everything downstream is canonical.
	return result.URL, canonicalHeaderKeys(result.Headers), nil
}

// remapFromHeaders encodes resolved headers the way the proxy query carries
// them (origin plain, cookies/xhdr as b64url).
func remapFromHeaders(u string, headers map[string]string) playlistRemap {
	origin := headers["Referer"]
	if origin == "" {
		origin = headers["Origin"]
	}
	rm := playlistRemap{url: u, origin: origin}
	if c := headers["Cookie"]; c != "" {
		rm.cookiesRaw = b64url(c)
	}
	if sfx := buildXhdrSuffix(b64url, headers); sfx != "" {
		// buildXhdrSuffix returns "&xhdr=<query-escaped b64>"; b64url needs no
		// escaping, so the value is the suffix minus its prefix.
		rm.xhdrRaw = strings.TrimPrefix(sfx, "&xhdr=")
	}
	return rm
}

// recoverChildPlaylist re-resolves the stream behind sidRaw and fetches child
// vi of the fresh master (or the resolved URL itself when it is already a
// media playlist). On success the new upstream is remembered for origURL.
func recoverChildPlaylist(client *http.Client, sidRaw, profileID, vi, origURL string, useVPN bool, egr string) (playlistRemap, []byte, error) {
	idx, err := strconv.Atoi(vi)
	if err != nil || idx < 0 {
		return playlistRemap{}, nil, fmt.Errorf("vi non valido %q", vi)
	}
	newURL, headers, err := reResolveShared(sidRaw, profileID)
	if err != nil {
		return playlistRemap{}, nil, fmt.Errorf("re-resolve: %w", err)
	}
	rm := remapFromHeaders(newURL, headers)
	top, err := fetchUpstreamPlaylist(client, rm.url, rm.origin, rm.cookiesRaw, rm.xhdrRaw, useVPN, egr)
	if err != nil {
		return playlistRemap{}, nil, fmt.Errorf("fetch nuovo master: %w", err)
	}
	content := top
	if isMasterPlaylist(top) {
		base := rm.url[:strings.LastIndex(rm.url, "/")+1]
		children := masterChildURIs(string(top), base)
		if idx >= len(children) {
			return playlistRemap{}, nil, fmt.Errorf("il nuovo master ha %d figli, richiesto vi=%d", len(children), idx)
		}
		rm.url = children[idx]
		content, err = fetchUpstreamPlaylist(client, rm.url, rm.origin, rm.cookiesRaw, rm.xhdrRaw, useVPN, egr)
		if err != nil {
			return playlistRemap{}, nil, fmt.Errorf("fetch nuova variante: %w", err)
		}
	}
	storePlaylistRemap(origURL, rm)
	return rm, content, nil
}
