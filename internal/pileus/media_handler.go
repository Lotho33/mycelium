package pileus

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// MediaHandler implements gen.MediaPipelineServer.
// Every method resolves a PipelineProvider via lookupBackend and delegates to it,
// keeping this file free of plugin-type branching.
type MediaHandler struct {
	gen.UnimplementedMediaPipelineServer
}

func NewMediaHandler() *MediaHandler { return &MediaHandler{} }

// parseHTTPHostHint interprets an x-http-host value from client metadata, which
// may be a bare host, a host:port, or a full origin (scheme://host[:port]). It
// returns the host[:port] to build proxy URLs against and the scheme it carried
// ("" if none). ok is false when the value is unusable — notably loopback:
// Pileus falls back to 127.0.0.1 when it can't resolve the server by name, and
// that is worthless for a URL a remote browser must reach.
func parseHTTPHostHint(v, defaultPort string) (hostPort, scheme string, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", "", false
	}
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || isLoopbackHost(u.Hostname()) {
			return "", "", false
		}
		return u.Host, u.Scheme, true
	}
	if host, port, err := net.SplitHostPort(v); err == nil {
		if isLoopbackHost(host) {
			return "", "", false
		}
		return net.JoinHostPort(host, port), "", true
	}
	if isLoopbackHost(v) {
		return "", "", false
	}
	return net.JoinHostPort(v, defaultPort), "", true
}

// splitServerHost parses the operator-set server_host / MYCELIUM_SERVER_HOST,
// which is trusted config and may be a bare host, host:port, or
// scheme://host[:port]. Unlike parseHTTPHostHint it does not reject loopback
// (the operator may legitimately point at localhost). A bare host keeps
// defaultPort, matching the historical behaviour.
func splitServerHost(v, defaultPort string) (hostPort, scheme string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			return "", ""
		}
		return u.Host, u.Scheme
	}
	if _, _, err := net.SplitHostPort(v); err == nil {
		return v, ""
	}
	return net.JoinHostPort(v, defaultPort), ""
}

func isLoopbackHost(h string) bool {
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return strings.EqualFold(h, "localhost")
}

// redactProxyURL drops the query from a generated /proxy/* URL before logging.
// That query carries the base64'd upstream URL and the upstream Cookie header —
// not secret-grade encoding, so it must never reach data/logs/*.log.
func redactProxyURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	return "(redacted)"
}

// ─────────────────────────────────────────────────────────────────────────────
// GetCatalog
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) GetCatalog(ctx context.Context, req *gen.CatalogRequest) (*gen.CatalogResponse, error) {
	page := max(int(req.Page), 1)

	// Shared response cache. The key (pluginID+catalogID+page) is
	// profile-independent — the same carousel opened by N devices/profiles
	// within the TTL shares one real upstream fetch instead of N. Skipped
	// entirely when Redis is unavailable (same as the count cache below).
	cacheKey := managers.CatalogCacheKey(req.PluginId, req.CatalogId, page)
	if managers.Redis != nil {
		if raw, gErr := managers.Redis.Get(ctx, cacheKey); gErr == nil && raw != "" {
			cached := &gen.CatalogResponse{}
			if proto.Unmarshal([]byte(raw), cached) == nil {
				if debugLog {
					log.Printf("[catalog-cache] hit %s/%s p%d (%d items)",
						req.PluginId, req.CatalogId, page, len(cached.Items))
				}
				updateCatalogCount(req.PluginId, req.CatalogId, page, len(cached.Items))
				return cached, nil
			}
			log.Printf("[catalog-cache] corrupt entry, refetching: %s", cacheKey)
		}
	}

	b, err := lookupBackend(req.PluginId)
	if err != nil {
		return nil, err
	}
	items, hasMore, err := b.GetCatalog(ctx, req.CatalogId, page)
	if err != nil {
		log.Printf("[pileus/media] GetCatalog %s/%s: %v", req.PluginId, req.CatalogId, err)
		return nil, wrapInternal(err)
	}

	resp := &gen.CatalogResponse{Items: items, HasMore: hasMore}

	// Write-through, fire-and-forget. TTL from the manifest's cache_ttl_seconds
	// when declared, else 15 min.
	if managers.Redis != nil {
		if blob, mErr := proto.Marshal(resp); mErr == nil {
			ttl := catalogCacheTTL(req.PluginId, req.CatalogId)
			wCtx, wCancel := context.WithTimeout(context.Background(), 2*time.Second)
			core.SafeGo("pileus/catalog-cache-store", func() {
				defer wCancel()
				if sErr := managers.Redis.Set(wCtx, cacheKey, string(blob), ttl); sErr == nil {
					log.Printf("[catalog-cache] stored %s/%s p%d (%d items, %d B, ttl %s)",
						req.PluginId, req.CatalogId, page, len(items), len(blob), ttl)
				}
			})
		}
	}

	updateCatalogCount(req.PluginId, req.CatalogId, page, len(items))
	return resp, nil
}

// updateCatalogCount refreshes the catalog count cache used by ListPlugins to
// hide empty carousels. Only page 1, only auto_hide_when_empty catalogs,
// fire-and-forget, no-op when Redis is down.
func updateCatalogCount(pluginID, catalogID string, page, count int) {
	if page != 1 || managers.Redis == nil {
		return
	}
	ttl := autoHideCatalogTTL(pluginID, catalogID)
	if ttl <= 0 {
		return
	}
	cCtx, cCancel := context.WithTimeout(context.Background(), 2*time.Second)
	core.SafeGo("pileus/catalog-count-store", func() {
		defer cCancel()
		_ = managers.Redis.Set(cCtx, managers.CatalogCountKey(pluginID, catalogID),
			strconv.Itoa(count), ttl)
	})
}

// catalogCacheTTL returns how long a GetCatalog response for this catalog may
// live in Redis: the manifest's cache_ttl_seconds when the catalog declares
// one, otherwise a 15-minute default (matches the key-convention comment in
// managers/redis.go). Always positive.
func catalogCacheTTL(pluginID, catalogID string) time.Duration {
	const def = 15 * time.Minute
	for _, mf := range engine.LuaPlugins.GetMeta() {
		if mf.ID != pluginID {
			continue
		}
		for _, c := range mf.Exposes.Catalogs {
			if c.ID == catalogID && c.CacheTTLSeconds > 0 {
				return time.Duration(c.CacheTTLSeconds) * time.Second
			}
		}
		break
	}
	return def
}

// autoHideCatalogTTL returns the TTL to use when caching an auto_hide count key.
// Returns 0 if the catalog is not auto_hide (skip caching).
// Handles both static manifest catalogs and dynamic catalog_list plugins.
func autoHideCatalogTTL(pluginID, catalogID string) time.Duration {
	for _, mf := range engine.LuaPlugins.GetMeta() {
		if mf.ID != pluginID {
			continue
		}
		// Check static manifest first (fast path, no Lua call).
		for _, c := range mf.Exposes.Catalogs {
			if c.ID == catalogID && c.AutoHideWhenEmpty {
				if c.CacheTTLSeconds > 0 {
					return time.Duration(c.CacheTTLSeconds) * time.Second
				}
				return 5 * time.Minute
			}
		}
		// Plugin has dynamic catalog_list — all its catalogs are auto_hide.
		if _, ok := mf.Entrypoints[engine.EPGetCatalogList]; ok {
			return 5 * time.Minute
		}
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// GetSearchFilters
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) GetSearchFilters(ctx context.Context, req *gen.SearchFiltersRequest) (*gen.SearchFiltersResponse, error) {
	b, err := lookupBackend(req.PluginId)
	if err != nil {
		// Unknown plugin → return empty filters rather than an error.
		return &gen.SearchFiltersResponse{}, nil
	}
	filters, err := b.GetSearchFilters(ctx)
	if err != nil {
		return &gen.SearchFiltersResponse{}, nil
	}
	return &gen.SearchFiltersResponse{Filters: filters}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Search
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) Search(ctx context.Context, req *gen.SearchRequest) (*gen.SearchResponse, error) {
	page := max(int(req.Page), 1)
	b, err := lookupBackend(req.PluginId)
	if err != nil {
		return nil, err
	}
	items, hasMore, err := b.Search(ctx, req.Query, page, req.Filters)
	if err != nil {
		return nil, wrapInternal(err)
	}
	return &gen.SearchResponse{Items: items, HasMore: hasMore}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GetDetails
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) GetDetails(ctx context.Context, req *gen.DetailsRequest) (*gen.DetailsResponse, error) {
	b, err := lookupBackend(req.PluginId)
	if err != nil {
		return nil, err
	}
	resp, err := b.GetDetails(ctx, req.MediaId)
	if err != nil {
		return nil, wrapInternal(err)
	}
	return resp, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Browse
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) Browse(ctx context.Context, req *gen.BrowseRequest) (*gen.BrowseResponse, error) {
	dirID := req.ParentId
	if req.ChildId != "" {
		dirID = req.ChildId
	}
	page := max(int(req.Page), 1)
	b, err := lookupBackend(req.PluginId)
	if err != nil {
		return nil, err
	}
	resp, err := b.Browse(ctx, dirID, page)
	if err != nil {
		return nil, wrapInternal(err)
	}
	return resp, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GetStreams
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) GetStreams(ctx context.Context, req *gen.StreamsRequest) (*gen.StreamsResponse, error) {
	b, err := lookupBackend(req.PluginId)
	if err != nil {
		return nil, err
	}
	sources, err := b.GetStreams(ctx, req.MediaId)
	if err != nil {
		return nil, wrapInternal(err)
	}
	return &gen.StreamsResponse{Sources: sources}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ResolveStream — raw resolve + HLS proxy + skip-time injection
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) ResolveStream(req *gen.ResolveRequest, stream gen.MediaPipeline_ResolveStreamServer) error {
	ctx := stream.Context()
	sendProgress := func(status, message string) error {
		return stream.Send(&gen.ResolveStreamEvent{
			Payload: &gen.ResolveStreamEvent_Progress{Progress: &gen.ResolveProgress{Message: message, Status: status}},
		})
	}
	// Plugin-driven progress (e.g. Lua's mycelium.progress()): forwarded live as
	// the plugin narrates what it's doing. Best-effort — a failed send here just
	// means the update is dropped, it doesn't abort the resolve.
	//
	// ctx.Err() guard: on a Lua-call timeout (90s, see callWithTimeout in
	// lua_plugin.go), the goroutine actually running the Lua call is NOT
	// stopped — Go can't force-kill a goroutine — it keeps running in the
	// background against a discarded LState while this RPC handler already
	// returns an error and the stream ends. If that abandoned goroutine still
	// calls mycelium.progress() afterwards, onProgress would fire and call
	// stream.Send on a stream whose handler has already returned — grpc-go
	// only guarantees Send is safe while the handler is still running. Once
	// the RPC's context is done, the framework is tearing the stream down;
	// skip the send instead of racing it.
	onProgress := func(status, message string) {
		if ctx.Err() != nil {
			return
		}
		if err := sendProgress(status, message); err != nil {
			log.Printf("[pileus/media] ResolveStream %s/%s: progress send: %v", req.PluginId, req.StreamId, err)
		}
	}

	b, err := lookupBackend(req.PluginId)
	if err != nil {
		return err
	}

	rawURL, headers, isLive, extra, err := b.ResolveStream(ctx, req.StreamId, onProgress)
	if err != nil {
		log.Printf("[pileus/media] ResolveStream %s/%s: %v", req.PluginId, req.StreamId, err)
		return wrapInternal(err)
	}
	// The browser sniff hands back header names lowercased ("cookie",
	// "user-agent"); the lookups below (headers["Cookie"], ["Referer"], ["Origin"])
	// and the xhdr skip-set are all title-cased. Canonicalise once so a captured
	// session-verification cookie (__cf_bm / cf_clearance) isn't silently dropped.
	headers = canonicalHeaderKeys(headers)

	urlLower := strings.ToLower(rawURL)
	isHLS := strings.Contains(urlLower, ".m3u8") || strings.Contains(urlLower, "/playlist/")

	// Parse skip-time metadata from extra (Lua plugins) or stream.Metadata (gRPC plugins).
	malID, epNum := 0, 0
	introdBKey, tmdbID, tmdbSeason, tmdbEpisode, mediaType := "", "", "", "", ""
	if extra != nil {
		if v := extra["mal_id"]; v != "" {
			malID, _ = strconv.Atoi(v)
		}
		if v := extra["ep_num"]; v != "" {
			epNum, _ = strconv.Atoi(v)
		}
		introdBKey = extra["introdb_api_key"]
		tmdbID = extra["tmdb_id"]
		tmdbSeason = extra["tmdb_season"]
		tmdbEpisode = extra["tmdb_episode"]
		mediaType = extra["media_type"]
	}

	extraOut := map[string]string{}
	useAniSkip := isHLS && malID > 0 && epNum > 0
	useIntroDb := isHLS && introdBKey != "" && tmdbID != ""
	if useAniSkip || useIntroDb {
		var cacheKey string
		if useAniSkip {
			cacheKey = fmt.Sprintf("skiptimes:anilist:%d:%d", malID, epNum)
		} else {
			cacheKey = fmt.Sprintf("skiptimes:introdb:%s:%s:%s:%s", tmdbID, mediaType, tmdbSeason, tmdbEpisode)
		}

		type skipData struct {
			DurationSec float64 `json:"duration_sec"`
			SkipTimes   string  `json:"skip_times"`
		}

		fromCache := false
		if managers.Redis != nil {
			if raw, err := managers.Redis.Get(ctx, cacheKey); err == nil && raw != "" {
				var sd skipData
				if json.Unmarshal([]byte(raw), &sd) == nil {
					fromCache = true
					if sd.SkipTimes != "" {
						extraOut["skip_times"] = sd.SkipTimes
					}
					if sd.DurationSec > 0 {
						extraOut["duration_sec"] = fmt.Sprintf("%.3f", sd.DurationSec)
					}
				}
			}
		}

		if !fromCache {
			if err := sendProgress("", "Ricerca tempi salta intro/outro…"); err != nil {
				return err
			}
			// The fetch runs in its own goroutine and always finishes (caching
			// the result for a future resolve of the same episode — replay,
			// resume, or a different device) regardless of what THIS resolve
			// does below. But previously nothing ever waited on it: this
			// resolve's {result} went out with no skip_times, every single
			// time, for every episode's very first watch — only a *second*
			// resolve of that same episode (a replay) ever hit the cache path
			// above and actually got skip markers. Reported 2026-09-18:
			// "i timestamp vengono caricati ma poi si perdono, non vengono
			// mostrati o usati" — exactly this: fetched (and cached), never
			// delivered to the session that asked for them.
			//
			// Give it a real but bounded chance instead: AniSkip/IntroDB is
			// normally sub-second, and the actual stream resolution below
			// (embed extraction etc.) already routinely takes several seconds
			// on its own — a short cap here is a good trade. On the rarer slow
			// upstream, this resolve still proceeds without markers after the
			// timeout (same as before this fix), the goroutine keeps running
			// and populates the cache regardless.
			//
			// The result is only ever written into extraOut from THIS
			// (calling) goroutine, never from the background one — extraOut
			// is read again further down (resp.Extra) without synchronization,
			// so a direct write from the goroutine after a timeout would be a
			// data race.
			resultCh := make(chan skipData, 1)
			core.SafeGo("pileus/skip-markers-enrich", func() {
				bgCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				dur := hlsDuration(rawURL, headers)
				var skip string
				if useAniSkip {
					skip = fetchAniSkip(malID, epNum, dur)
					log.Printf("[pileus/aniskip] mal=%d ep=%d dur=%.1fs intervals=%v", malID, epNum, dur, skip != "")
				}
				if skip == "" && useIntroDb {
					skip = fetchIntroDb(introdBKey, tmdbID, mediaType, tmdbSeason, tmdbEpisode, int64(dur*1000))
				}
				if managers.Redis != nil && (dur > 0 || skip != "") {
					if b, err := json.Marshal(skipData{DurationSec: dur, SkipTimes: skip}); err == nil {
						_ = managers.Redis.Set(bgCtx, cacheKey, string(b), 30*24*time.Hour)
					}
				}
				resultCh <- skipData{DurationSec: dur, SkipTimes: skip}
			})
			select {
			case sd := <-resultCh:
				if sd.SkipTimes != "" {
					extraOut["skip_times"] = sd.SkipTimes
				}
				if sd.DurationSec > 0 {
					extraOut["duration_sec"] = fmt.Sprintf("%.3f", sd.DurationSec)
				}
			case <-time.After(4 * time.Second):
				log.Printf("[pileus/aniskip] mal=%d ep=%d: fetch still running after 4s, continuing without skip markers this time (cached for next)", malID, epNum)
			}
		}
	}

	// Stream semaphore + session tracking.
	userID := profileIDFromCtx(ctx)
	if userID == "" {
		userID, _ = ctx.Value(ctxKeyDeviceID{}).(string)
	}
	if userID != "" {
		totalSec := 0.0
		if ds, ok := extraOut["duration_sec"]; ok {
			totalSec, _ = strconv.ParseFloat(ds, 64)
		}
		managers.StreamSem.Acquire(userID, req.StreamId)
		managers.Sessions.Register(userID, &managers.StreamSession{
			UserID:   userID,
			PluginID: req.PluginId,
			SourceID: req.StreamId,
			IsLive:   isLive,
			TotalSec: totalSec,
		})
	}

	// Plugin che dichiarano direct_stream: nessun proxy, l'URL risolto va al
	// player così com'è. Evita sia il limite di 60s sull'intera richiesta di
	// getSegmentClient (internal/api/proxy.go — pensato per singoli segmenti
	// TS, letale per un intero file in direct-play come quelli di jellyfin)
	// sia la perdita del supporto nativo a Range/seek dell'origine.
	if engine.LuaPlugins.ShouldSkipProxy(req.PluginId) {
		resp := &gen.ResolveResponse{ResolvedUrl: rawURL, IsLive: isLive}
		if headers != nil {
			resp.HttpHeaders = headers
		}
		if len(extraOut) > 0 {
			resp.Extra = extraOut
		}
		return stream.Send(&gen.ResolveStreamEvent{Payload: &gen.ResolveStreamEvent_Result{Result: resp}})
	}

	// Il player non riceve mai un URL upstream grezzo: sia HLS che non-HLS
	// passano sempre dal proxy di mycelium (bypassa CORS per il player e tiene
	// token/cookie upstream fuori dal client). vpnParam marca inoltre la
	// richiesta per il tunnel VPN quando il flusso video di questo plugin è
	// configurato per usarlo (vedi engine.LuaPluginManager.ShouldUseVPNForVideo).
	httpPort := managers.Settings.GetString("server_port", "8000")
	// proxyScheme/proxyHost = come QUESTO client raggiunge il proxy HTTP di
	// mycelium. Ordine di priorità, dal più debole al più forte:
	//
	//  1. http://127.0.0.1 — ultima spiaggia (player sulla stessa macchina).
	//  2. x-http-host / x-http-scheme — hint del client. Sul percorso gRPC-web
	//     il bridge li sintetizza dagli header Host / X-Forwarded-Host /
	//     X-Forwarded-Proto della PWA (internal/pileus/grpcweb.go), così l'URL
	//     proxy usa la stessa origine con cui il browser ci ha raggiunti (niente
	//     mixed content: se il browser è su https, l'URL esce https). x-http-host
	//     accetta host, host:porta o un'origine completa; loopback è ignorato.
	//  3. peer.LocalAddr   — IP locale della connessione gRPC nativa = l'indirizzo
	//     che il client ha davvero usato. Corretto con network_mode: host (o
	//     bare-metal); in bridge mode è l'IP veth (es. 172.18.0.5) — inutile,
	//     ma in bridge serve comunque (4). Sul percorso gRPC-web è sempre
	//     loopback (il bridge dialoga da 127.0.0.1) quindi non calpesta (2).
	//  4. server_host / MYCELIUM_SERVER_HOST — override esplicito. Obbligatorio
	//     solo in bridge mode (né (2) né (3) danno l'IP host-published); con
	//     network_mode: host non serve. Può includere lo schema, es.
	//     "https://media.example.com". Dashboard → "Host del server".
	proxyScheme := "http"
	proxyHost := "127.0.0.1:" + httpPort
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-http-scheme"); len(vals) > 0 && (vals[0] == "http" || vals[0] == "https") {
			proxyScheme = vals[0]
		}
		if vals := md.Get("x-http-host"); len(vals) > 0 && vals[0] != "" {
			if host, scheme, hok := parseHTTPHostHint(vals[0], httpPort); hok {
				proxyHost = host
				if scheme != "" {
					proxyScheme = scheme
				}
			}
		}
	}
	if pr, ok := peer.FromContext(ctx); ok && pr.LocalAddr != nil {
		if host, _, err := net.SplitHostPort(pr.LocalAddr.String()); err == nil {
			if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
				proxyHost = net.JoinHostPort(host, httpPort)
			}
		}
	}
	serverHost := managers.Settings.GetString("server_host", "")
	if serverHost == "" {
		serverHost = os.Getenv("MYCELIUM_SERVER_HOST")
	}
	if serverHost != "" {
		if host, scheme := splitServerHost(serverHost, httpPort); host != "" {
			proxyHost = host
			if scheme != "" {
				proxyScheme = scheme
			}
		}
	}
	b64 := func(s string) string {
		return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
	}
	uidParam := ""
	if userID != "" {
		uidParam = "&uid=" + url.QueryEscape(userID)
	}
	origin := ""
	if headers != nil {
		if v := headers["Referer"]; v != "" {
			origin = v
		} else {
			origin = headers["Origin"]
		}
	}
	cookie := ""
	if headers != nil {
		cookie = headers["Cookie"]
	}
	// Codifica gli header extra catturati dal browser (UA, Accept, ecc.) come xhdr
	// così il proxy li invia al CDN identici a Chrome — necessario per admin streams.
	xhdrParam := ""
	if headers != nil {
		skipH := map[string]bool{
			"Referer": true, "referer": true,
			"Cookie": true, "cookie": true,
			"Origin": true, "origin": true,
		}
		extra := make(map[string]string)
		for k, v := range headers {
			if !skipH[k] {
				extra[k] = v
			}
		}
		if len(extra) > 0 {
			if j, jerr := json.Marshal(extra); jerr == nil {
				xhdrParam = "&xhdr=" + url.QueryEscape(b64(string(j)))
			}
		}
	}
	// vpnParam pins every proxied playlist/segment/key URL to the egress
	// profile the operator picked for this plugin (managers.EgressProfiles):
	// "&vpn=1&egr=<name>", or empty when the plugin's video flow goes direct.
	vpnParam := ""
	if egr := engine.LuaPlugins.PluginEgress(req.PluginId); egr != "" && egr != managers.EgressDirect {
		vpnParam = "&vpn=1&egr=" + url.QueryEscape(egr)
	}

	if isHLS {
		// Embed a sid so the proxy can re-resolve when the CDN token expires or
		// is rejected (calls resolve_stream again via proxyResolveForPlaylist).
		// Not just for live streams: VOD CDN tokens can also be rejected
		// (expired, IP/fingerprint mismatch) and benefit from a retry.
		sidParam := ""
		if req.PluginId != "" && req.StreamId != "" {
			sid := b64(req.PluginId + "\x00" + req.StreamId)
			sidParam = "&sid=" + url.QueryEscape(sid)
		}
		proxyURL := fmt.Sprintf("%s://%s/proxy/playlist.m3u8?data=%s&origin=%s&cookies=%s%s%s%s%s",
			proxyScheme, proxyHost, b64(rawURL), b64(origin), b64(cookie), sidParam, uidParam, xhdrParam, vpnParam)
		proxyURL = core.AppendProxySig(proxyURL)
		log.Printf("[pileus/media] ResolveStream: HLS→proxy %s", redactProxyURL(proxyURL))

		// Pre-buffer the head of the stream before returning {result}: warm the
		// media playlist + first few segments into the proxy cache and narrate
		// the wait to the client ({progress}, status "loading", verbatim
		// message) instead of leaving it on a blind spinner for the whole
		// segment-download phase. Best-effort and time-capped — it never hangs
		// the RPC. See internal/pileus/prebuffer.go and docs/pileus-contract.md.
		// Best-effort: any pre-buffer hiccup (including an interactive
		// verification hit on the playlist) is swallowed here — the RPC
		// still returns {result}, playback just starts without a warm cache.
		_ = prebufferHLS(ctx, req.PluginId, rawURL, headers, isLive, sendProgress)
		if cerr := ctx.Err(); cerr != nil {
			// User backed out mid-pre-buffer — the stream is being torn down;
			// return the context error rather than Send onto a dead stream.
			return status.FromContextError(cerr).Err()
		}

		return stream.Send(&gen.ResolveStreamEvent{
			Payload: &gen.ResolveStreamEvent_Result{
				Result: &gen.ResolveResponse{ResolvedUrl: proxyURL, IsLive: isLive, Extra: extraOut},
			},
		})
	}

	proxyURL := fmt.Sprintf("%s://%s/proxy/segment.ts?data=%s&origin=%s&cookies=%s%s%s%s",
		proxyScheme, proxyHost, b64(rawURL), b64(origin), b64(cookie), uidParam, xhdrParam, vpnParam)
	proxyURL = core.AppendProxySig(proxyURL)
	log.Printf("[pileus/media] ResolveStream: non-HLS→proxy %s", redactProxyURL(proxyURL))
	// HttpHeaders is deliberately omitted: those were meant for the upstream
	// CDN (already embedded in proxyURL's cookies/origin/xhdr params) — the
	// player now only ever talks to mycelium's own proxy.
	resp := &gen.ResolveResponse{
		ResolvedUrl: proxyURL,
		IsLive:      isLive,
	}
	if len(extraOut) > 0 {
		resp.Extra = extraOut
	}
	return stream.Send(&gen.ResolveStreamEvent{Payload: &gen.ResolveStreamEvent_Result{Result: resp}})
}

// ─────────────────────────────────────────────────────────────────────────────
// UpdateProgress
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) UpdateProgress(ctx context.Context, req *gen.ProgressRequest) (*gen.ProgressResponse, error) {
	deviceID, _ := ctx.Value(ctxKeyDeviceID{}).(string)
	clientID := deviceID
	if clientID == "" {
		clientID = "unknown"
	}
	if pid := profileIDFromCtx(ctx); pid != "" {
		if !profileExists(pid) {
			return nil, status.Error(codes.NotFound, "unknown profile")
		}
		clientID = pid
	}
	err := managers.DB.UpsertProgress(
		clientID, req.PluginId, req.MediaId, req.ParentId,
		req.NavigationContext, req.Title, req.Poster,
		float64(req.CurrentPosition), float64(req.TotalDuration),
		req.Rating, req.Genres, req.Plot, req.Year,
	)
	if err != nil {
		log.Printf("[pileus/media] UpdateProgress: %v", err)
	}
	return &gen.ProgressResponse{Ok: err == nil}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteProgress
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) DeleteProgress(ctx context.Context, req *gen.DeleteProgressRequest) (*gen.DeleteProgressResponse, error) {
	deviceID, _ := ctx.Value(ctxKeyDeviceID{}).(string)
	clientID := deviceID
	if clientID == "" {
		clientID = "unknown"
	}
	if pid := profileIDFromCtx(ctx); pid != "" {
		if !profileExists(pid) {
			return nil, status.Error(codes.NotFound, "unknown profile")
		}
		clientID = pid
	}
	err := managers.DB.DeleteProgress(clientID, req.PluginId, req.MediaId)
	if err != nil {
		log.Printf("[pileus/media] DeleteProgress: %v", err)
	}
	return &gen.DeleteProgressResponse{Ok: err == nil}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GetContinueWatching
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) GetContinueWatching(ctx context.Context, req *gen.ContinueWatchingRequest) (*gen.ContinueWatchingResponse, error) {
	deviceID, _ := ctx.Value(ctxKeyDeviceID{}).(string)
	clientID := deviceID
	if clientID == "" {
		clientID = "unknown"
	}
	if pid := profileIDFromCtx(ctx); pid != "" {
		if !profileExists(pid) {
			return nil, status.Error(codes.NotFound, "unknown profile")
		}
		clientID = pid
	}
	limit := int(req.Limit)
	if limit <= 0 {
		limit = 20
	}

	entries, err := managers.DB.GetContinueWatching(clientID, limit, req.ParentId)
	if err != nil {
		log.Printf("[pileus/media] GetContinueWatching: %v", err)
		return nil, status.Error(codes.Internal, "errore recupero cronologia")
	}

	items := make([]*gen.ContinueWatchingItem, 0, len(entries))
	for _, e := range entries {
		if req.PluginId != "" && e.ProviderID != req.PluginId {
			continue
		}
		items = append(items, &gen.ContinueWatchingItem{
			PluginId:          e.ProviderID,
			MediaId:           e.PlayableID,
			ParentId:          e.ParentId,
			NavigationContext: e.NavigationContext,
			Title:             e.Title,
			Poster:            e.Poster,
			ProgressTime:      e.ProgressTime,
			TotalTime:         e.TotalTime,
			LastUpdated:       e.LastUpdated,
			Rating:            e.Rating,
			Genres:            e.Genres,
			Plot:              e.Plot,
			Year:              e.Year,
		})
	}
	return &gen.ContinueWatchingResponse{Items: items}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ListPlugins
// ─────────────────────────────────────────────────────────────────────────────

func (h *MediaHandler) ListPlugins(ctx context.Context, _ *gen.PluginListRequest) (*gen.PluginListResponse, error) {
	var plugins []*gen.PluginInfo

	luaMetas := engine.LuaPlugins.GetMeta()
	luaIDs := make([]string, 0, len(luaMetas))
	for _, mf := range luaMetas {
		if engine.LuaPlugins.Has(mf.ID) {
			luaIDs = append(luaIDs, mf.ID)
		}
	}
	// Single MGET instead of N individual Redis GETs.
	statusMap := engine.LuaPlugins.GetStatusBatch(luaIDs)

	// Resolve catalog lists — static from manifest or dynamic via catalog_list entrypoint.
	resolvedCatalogs := make(map[string][]engine.LuaCatalogDef, len(luaMetas))
	for _, mf := range luaMetas {
		if engine.LuaPlugins.Has(mf.ID) {
			resolvedCatalogs[mf.ID] = engine.LuaPlugins.ResolveCatalogDefs(mf.ID)
		}
	}

	// Pre-fetch catalog counts (auto_hide_when_empty) in a single MGET.
	// isKnownEmpty[key] == true means the last page-1 fetch returned 0 items.
	isKnownEmpty := buildEmptyCatalogSet(ctx, resolvedCatalogs)

	for _, mf := range luaMetas {
		if !engine.LuaPlugins.Has(mf.ID) {
			continue
		}
		if !engine.LuaPlugins.IsOperational(mf.ID) {
			continue
		}

		rs := statusMap[mf.ID]
		reachable, lastOkUnix := engine.LuaPlugins.GetReachability(mf.ID)

		liveRefreshable := false
		for _, t := range mf.Tasks {
			if t.LiveRefresh {
				liveRefreshable = true
				break
			}
		}

		needsConfig := false
		for _, f := range mf.Settings.Global {
			if f.Required && managers.Settings.GetString("lua:"+mf.ID+":global:"+f.ID, "") == "" {
				needsConfig = true
				break
			}
		}

		// Surface search-filter support as a capability so Pileus can hide the
		// filter UI for plugins that don't implement it. Derived from the
		// manifest's own entrypoint declaration — single source of truth, can't
		// drift.
		caps := mf.Exposes.Capabilities
		if mf.Entrypoints[engine.EPGetSearchFilters] != "" {
			caps = append(append([]string(nil), caps...), "search_filters")
		}

		pi := &gen.PluginInfo{
			PluginId:     mf.ID,
			Name:         mf.Name,
			Version:      mf.Version,
			IsReady:      !needsConfig && rs.Label != engine.StatusError,
			Capabilities: caps,
			StatusLabel:  rs.Label,
			StatusDetail: rs.Detail,
			NeedsConfig:  needsConfig,
			Reachable:    reachable,
			LastOkUnix:   lastOkUnix,
		}
		for _, c := range resolvedCatalogs[mf.ID] {
			if c.AutoHideWhenEmpty && isKnownEmpty[managers.CatalogCountKey(mf.ID, c.ID)] {
				continue
			}
			pi.Catalogs = append(pi.Catalogs, &gen.CatalogDef{
				Id:                    c.ID,
				Name:                  c.Name,
				Type:                  c.Type,
				CacheTtlSeconds:       c.CacheTTLSeconds,
				SectionKind:           c.SectionKind,
				StyleHint:             c.StyleHint,
				LiveRefreshable:       liveRefreshable,
				CardLayout:            c.CardLayout,
				DisableHeroBackground: c.DisableHeroBackground,
			})
		}
		plugins = append(plugins, pi)
	}

	return &gen.PluginListResponse{Plugins: plugins}, nil
}

// buildEmptyCatalogSet returns a set of CatalogCountKeys whose last known
// page-1 count is 0. Uses a single MGET across all auto_hide_when_empty
// catalogs in all Lua plugins.
func buildEmptyCatalogSet(ctx context.Context, pluginCatalogs map[string][]engine.LuaCatalogDef) map[string]bool {
	if managers.Redis == nil {
		return nil
	}
	type entry struct{ key string }
	var entries []entry
	for pluginID, cats := range pluginCatalogs {
		for _, c := range cats {
			if c.AutoHideWhenEmpty {
				entries = append(entries, entry{managers.CatalogCountKey(pluginID, c.ID)})
			}
		}
	}
	if len(entries) == 0 {
		return nil
	}
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.key
	}
	mCtx, mCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer mCancel()
	vals, err := managers.Redis.MGet(mCtx, keys...)
	if err != nil {
		return nil
	}
	result := make(map[string]bool, len(vals))
	for i, v := range vals {
		if v == "0" {
			result[keys[i]] = true
		}
	}
	return result
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func profileIDFromCtx(ctx context.Context) string {
	pid, _ := ctx.Value(ctxKeyProfileID{}).(string)
	return pid
}

// wrapInternal passes gRPC status errors through unchanged and wraps plain
// errors in codes.Internal. This lets backends signal NotFound or other codes
// without the handler needing to inspect the error type.
func wrapInternal(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}

// ─────────────────────────────────────────────────────────────────────────────
// HLS duration + skip-time helpers (AniSkip v2, TheIntroDB v3)
// ─────────────────────────────────────────────────────────────────────────────

var aniSkipClient = &http.Client{Timeout: 6 * time.Second}

// hlsDuration fetches an HLS playlist and sums #EXTINF durations.
func hlsDuration(masterURL string, headers map[string]string) float64 {
	fetch := func(u string) ([]byte, error) {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := aniSkipClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}

	masterBody, err := fetch(masterURL)
	if err != nil {
		log.Printf("[pileus/hls] master fetch error: %v", err)
		return 0
	}
	log.Printf("[pileus/hls] master body (first 300): %.300s", string(masterBody))

	subURL := ""
	baseURL := masterURL[:strings.LastIndex(masterURL, "/")+1]
	scanner := bufio.NewScanner(strings.NewReader(string(masterBody)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "http") {
			subURL = line
		} else {
			subURL = baseURL + line
		}
		break
	}
	if subURL == "" {
		subURL = masterURL
	}
	log.Printf("[pileus/hls] sub playlist: %s", subURL)

	subBody, err := fetch(subURL)
	if err != nil {
		log.Printf("[pileus/hls] sub fetch error: %v", err)
		return 0
	}

	var total float64
	var segments int
	scanner = bufio.NewScanner(strings.NewReader(string(subBody)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		line = strings.TrimPrefix(line, "#EXTINF:")
		if idx := strings.IndexAny(line, ",\n"); idx >= 0 {
			line = line[:idx]
		}
		if d, err := strconv.ParseFloat(line, 64); err == nil {
			total += d
			segments++
		}
	}
	log.Printf("[pileus/hls] segments=%d total=%.3fs", segments, total)
	return total
}

type skipInterval struct {
	SkipType string `json:"skipType"`
	Interval struct {
		StartTime float64 `json:"startTime"`
		EndTime   float64 `json:"endTime"`
	} `json:"interval"`
}

func fetchIntroDb(apiKey, tmdbID, mediaType, season, episode string, durationMs int64) string {
	if apiKey == "" || tmdbID == "" {
		return ""
	}
	var rawURL string
	if mediaType == "tv" {
		rawURL = fmt.Sprintf(
			"https://api.theintrodb.org/v3/media?tmdb_id=%s&season=%s&episode=%s&duration_ms=%d",
			tmdbID, season, episode, durationMs,
		)
	} else {
		rawURL = fmt.Sprintf(
			"https://api.theintrodb.org/v3/media?tmdb_id=%s&duration_ms=%d",
			tmdbID, durationMs,
		)
	}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-Api-Key", apiKey)
	log.Printf("[pileus/introdb] GET %s", rawURL)
	resp, err := aniSkipClient.Do(req)
	if err != nil {
		log.Printf("[pileus/introdb] request error: %v", err)
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	log.Printf("[pileus/introdb] HTTP %d", resp.StatusCode)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}

	var d struct {
		Intro []struct {
			StartMs *int64 `json:"start_ms"`
			EndMs   *int64 `json:"end_ms"`
		} `json:"intro"`
		Credits []struct {
			StartMs *int64 `json:"start_ms"`
			EndMs   *int64 `json:"end_ms"`
		} `json:"credits"`
		Recap []struct {
			StartMs *int64 `json:"start_ms"`
			EndMs   *int64 `json:"end_ms"`
		} `json:"recap"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return ""
	}

	type outInterval struct {
		SkipType string `json:"skipType"`
		Interval struct {
			StartTime float64 `json:"startTime"`
			EndTime   float64 `json:"endTime"`
		} `json:"interval"`
	}

	ms2s := func(ms *int64, fallback float64) float64 {
		if ms == nil {
			return fallback
		}
		return float64(*ms) / 1000.0
	}
	durSec := float64(durationMs) / 1000.0

	var results []outInterval
	for _, seg := range d.Intro {
		start, end := ms2s(seg.StartMs, 0), ms2s(seg.EndMs, durSec)
		if end > start {
			var r outInterval
			r.SkipType = "op"
			r.Interval.StartTime = start
			r.Interval.EndTime = end
			results = append(results, r)
		}
	}
	for _, seg := range d.Credits {
		start, end := ms2s(seg.StartMs, 0), ms2s(seg.EndMs, durSec)
		if end > start {
			var r outInterval
			r.SkipType = "ed"
			r.Interval.StartTime = start
			r.Interval.EndTime = end
			results = append(results, r)
		}
	}
	for _, seg := range d.Recap {
		start, end := ms2s(seg.StartMs, 0), ms2s(seg.EndMs, durSec)
		if end > start {
			var r outInterval
			r.SkipType = "recap"
			r.Interval.StartTime = start
			r.Interval.EndTime = end
			results = append(results, r)
		}
	}
	if len(results) == 0 {
		return ""
	}
	out, _ := json.Marshal(results)
	log.Printf("[pileus/introdb] found %d intervals", len(results))
	return string(out)
}

func fetchAniSkip(malID, epNum int, durationSec float64) string {
	if malID <= 0 || epNum <= 0 {
		return ""
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("https://api.aniskip.com/v2/skip-times/%d/%d", malID, epNum), nil)
	if err != nil {
		return ""
	}
	req.URL.RawQuery = fmt.Sprintf(
		"types[]=op&types[]=ed&types[]=recap&episodeLength=%d",
		int(durationSec),
	)
	log.Printf("[pileus/aniskip] GET %s", req.URL.String())
	resp, err := aniSkipClient.Do(req)
	if err != nil {
		log.Printf("[pileus/aniskip] request error: %v", err)
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	log.Printf("[pileus/aniskip] HTTP %d body=%s", resp.StatusCode, string(body))
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	var wrapper struct {
		Results []skipInterval `json:"results"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil || len(wrapper.Results) == 0 {
		return ""
	}
	out, _ := json.Marshal(wrapper.Results)
	return string(out)
}

// canonicalHeaderKeys returns h with every key run through
// http.CanonicalHeaderKey ("cookie" → "Cookie", "user-agent" → "User-Agent").
// On a case-collision a non-empty value wins over an empty one, otherwise the
// last write wins. Returns nil for a nil map.
func canonicalHeaderKeys(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		ck := http.CanonicalHeaderKey(k)
		if ex, ok := out[ck]; ok && ex != "" && v == "" {
			continue
		}
		out[ck] = v
	}
	return out
}
