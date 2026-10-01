package pileus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/downloads"
	"mycelium/internal/engine"
	"mycelium/internal/managers"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// MediaHandler implements gen.MediaPipelineServer; every method delegates to
// the plugin's PipelineProvider (lookupBackend).
type MediaHandler struct {
	gen.UnimplementedMediaPipelineServer
}

func NewMediaHandler() *MediaHandler { return &MediaHandler{} }

// parseHTTPHostHint interprets a client's x-http-host (host, host:port or
// scheme://host[:port]) and returns the host[:port] and scheme ("" if none)
// to build proxy URLs with. ok is false for loopback, useless to a remote
// player.
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

// splitServerHost parses the operator-set server_host / MYCELIUM_SERVER_HOST
// (host, host:port or scheme://host[:port]; loopback allowed). A bare host
// gets defaultPort.
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

// redactProxyURL drops the query of a /proxy/* URL before logging: it
// carries the upstream URL and cookies.
func redactProxyURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	return "(redacted)"
}

// upstreamHost is all that gets logged of an upstream URL: path and query
// often carry tokens.
func upstreamHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "(url non valido)"
}

func (h *MediaHandler) GetCatalog(ctx context.Context, req *gen.CatalogRequest) (*gen.CatalogResponse, error) {
	page := max(int(req.Page), 1)

	// Shared, profile-independent response cache (pluginID+catalogID+page).
	// Skipped without Redis.
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

	// Write-through, fire-and-forget; TTL from cache_ttl_seconds, else 15 min.
	// Empty pages aren't cached: they are as often a transient failure.
	if managers.Redis != nil && len(items) > 0 {
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

// updateCatalogCount refreshes the page-1 item count ListPlugins uses to
// hide empty auto_hide_when_empty catalogs (fire-and-forget, needs Redis).
// emptyCatalogCountTTL caps how long a zero count (carousel hidden) is kept.
const emptyCatalogCountTTL = 2 * time.Minute

func updateCatalogCount(pluginID, catalogID string, page, count int) {
	if page != 1 || managers.Redis == nil {
		return
	}
	ttl := autoHideCatalogTTL(pluginID, catalogID)
	if ttl <= 0 {
		return
	}
	// A zero count hides the carousel: keep it short-lived.
	if count == 0 && ttl > emptyCatalogCountTTL {
		ttl = emptyCatalogCountTTL
	}
	cCtx, cCancel := context.WithTimeout(context.Background(), 2*time.Second)
	core.SafeGo("pileus/catalog-count-store", func() {
		defer cCancel()
		_ = managers.Redis.Set(cCtx, managers.CatalogCountKey(pluginID, catalogID),
			strconv.Itoa(count), ttl)
	})
}

// catalogCacheTTL is the cache lifetime of a catalog page: the manifest's
// cache_ttl_seconds, else 15 minutes.
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

// autoHideCatalogTTL returns the TTL of an auto_hide count key, 0 when the
// catalog isn't auto_hide. Handles static and catalog_list catalogs.
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
		// Dynamic catalog_list: all its catalogs are auto_hide.
		if _, ok := mf.Entrypoints[engine.EPGetCatalogList]; ok {
			return 5 * time.Minute
		}
	}
	return 0
}

func (h *MediaHandler) GetSearchFilters(ctx context.Context, req *gen.SearchFiltersRequest) (*gen.SearchFiltersResponse, error) {
	b, err := lookupBackend(req.PluginId)
	if err != nil {
		// Unknown plugin: empty filters, not an error.
		return &gen.SearchFiltersResponse{}, nil
	}
	filters, err := b.GetSearchFilters(ctx)
	if err != nil {
		return &gen.SearchFiltersResponse{}, nil
	}
	return &gen.SearchFiltersResponse{Filters: filters}, nil
}

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

func (h *MediaHandler) ResolveStream(req *gen.ResolveRequest, stream gen.MediaPipeline_ResolveStreamServer) error {
	ctx := stream.Context()
	sender := newResolveSender(stream)
	defer sender.close()
	stopKeepAlive := sender.keepAlive(ctx, resolveKeepAliveEvery)
	defer stopKeepAlive()
	sendProgress := sender.progress
	// Plugin progress (mycelium.progress) is forwarded live, best-effort. Once
	// ctx is done the stream is being torn down: a timed-out Lua goroutine may
	// still call progress, so the send is skipped.
	onProgress := func(status, message string) {
		if ctx.Err() != nil {
			return
		}
		if err := sendProgress(status, message); err != nil {
			log.Printf("[pileus/media] ResolveStream %s/%s: progress send: %v", req.PluginId, req.StreamId, err)
		}
	}

	// One device playing per profile (managers.PlaybackLeases): checked before
	// the plugin runs, claimed once the resolve succeeds.
	deviceID, _ := ctx.Value(ctxKeyDeviceID{}).(string)
	profileID := profileIDFromCtx(ctx)
	if holder := managers.PlaybackLeases.Holder(profileID, deviceID); holder != "" {
		if !req.TakeOver {
			return playbackInUseError(holder)
		}
		// "Watch here": the other device learns it from its next /proxy fetch (409)
		// or heartbeat (playback_elsewhere).
		managers.PlaybackLeases.TakeOver(profileID, deviceID)
		log.Printf("[pileus/media] profilo %s: riproduzione spostata da %s a %s", profileID, holder, deviceID)
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
	// Sniffed header names can be lowercase; the lookups below are canonical.
	headers = canonicalHeaderKeys(headers)

	urlLower := strings.ToLower(rawURL)
	isHLS := strings.Contains(urlLower, ".m3u8") || strings.Contains(urlLower, "/playlist/")

	// The plugin's extra fields reach the client untouched: their meaning is
	// between plugin and client.
	extraOut := map[string]string{}
	for k, v := range extra {
		extraOut[k] = v
	}

	// Playback lease + session tracking. The session is keyed by a random token
	// (the uid of every /proxy URL minted below), so two devices on one profile
	// never share a session.
	userID := profileID
	if userID == "" {
		userID = deviceID
	}
	sessionToken := ""
	if userID != "" {
		if holder, ok := managers.PlaybackLeases.Claim(profileID, deviceID); !ok {
			return playbackInUseError(holder)
		}
		totalSec := 0.0
		if ds, ok := extraOut["duration_sec"]; ok {
			totalSec, _ = strconv.ParseFloat(ds, 64)
		}
		sessionToken = newID()
		managers.Sessions.Register(sessionToken, &managers.StreamSession{
			UserID:    userID,
			ProfileID: profileID,
			DeviceID:  deviceID,
			PluginID:  req.PluginId,
			SourceID:  req.StreamId,
			IsLive:    isLive,
			TotalSec:  totalSec,
		})
	}

	// direct_stream plugins: the resolved URL goes to the player as-is (keeps
	// the origin's native Range support, no proxy time limits).
	if engine.LuaPlugins.ShouldSkipProxy(req.PluginId) {
		resp := &gen.ResolveResponse{ResolvedUrl: rawURL, IsLive: isLive}
		if headers != nil {
			resp.HttpHeaders = headers
		}
		if len(extraOut) > 0 {
			resp.Extra = extraOut
		}
		return sender.result(resp)
	}

	// The player never gets a raw upstream URL: everything goes through the
	// proxy, which handles CORS and keeps upstream tokens/cookies server-side.
	proxyScheme, proxyHost := clientProxyBase(ctx)
	proxyURL := mintProxyURL(proxyScheme, proxyHost, req.PluginId, req.StreamId, rawURL, headers, sessionToken, isHLS)

	if isHLS {
		log.Printf("[pileus/media] ResolveStream: HLS→proxy %s", redactProxyURL(proxyURL))

		// Pre-buffer the head of the stream before {result}: warm the playlist and
		// first segments into the proxy cache and narrate the wait ({progress},
		// status "loading"). Best-effort and time-capped: any failure just means no
		// warm cache. See prebuffer.go and docs/pileus-contract.md.
		_ = prebufferHLS(ctx, req.PluginId, rawURL, headers, isLive, req.StartPositionSec, sendProgress)
		if cerr := ctx.Err(); cerr != nil {
			// The user backed out: don't Send on a dead stream.
			return status.FromContextError(cerr).Err()
		}

		return sender.result(&gen.ResolveResponse{ResolvedUrl: proxyURL, IsLive: isLive, Extra: extraOut})
	}

	log.Printf("[pileus/media] ResolveStream: non-HLS→proxy %s", redactProxyURL(proxyURL))
	// No HttpHeaders: they are for the upstream and already travel inside the
	// proxy URL.
	resp := &gen.ResolveResponse{
		ResolvedUrl: proxyURL,
		IsLive:      isLive,
	}
	if len(extraOut) > 0 {
		resp.Extra = extraOut
	}
	return sender.result(resp)
}

// clientProxyBase is how the calling client reaches mycelium's HTTP server
// (scheme, host:port), for /proxy and download URLs.
func clientProxyBase(ctx context.Context) (proxyScheme, proxyHost string) {
	httpPort := managers.Settings.GetString("server_port", "8000")
	// Precedence, weakest first:
	//  1. http://127.0.0.1 — last resort (player on the same machine).
	//  2. x-http-host / x-http-scheme — the client's hint; on the gRPC-web path
	//     the bridge derives them from Host / X-Forwarded-*, so the URL uses the
	//     browser's own origin and scheme. Loopback is ignored.
	//  3. peer.LocalAddr — the local address of the native gRPC connection, i.e.
	//     what the client dialed (correct with host networking; a veth address
	//     in bridge mode). Always loopback on the gRPC-web path.
	//  4. server_host / MYCELIUM_SERVER_HOST — explicit override, required in
	//     bridge mode; may include a scheme ("https://media.example.com").
	proxyScheme = "http"
	proxyHost = "127.0.0.1:" + httpPort
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
	return proxyScheme, proxyHost
}

// mintProxyURL builds the signed /proxy URL of a resolved stream: the
// playlist proxy for HLS (with a sid, so the proxy can re-resolve an
// expired token), the segment proxy otherwise. sessionToken becomes the uid
// ("" for none).
func mintProxyURL(proxyScheme, proxyHost, pluginID, streamID, rawURL string, headers map[string]string, sessionToken string, isHLS bool) string {
	b64 := func(s string) string {
		return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
	}
	uidParam := ""
	if sessionToken != "" {
		uidParam = "&uid=" + url.QueryEscape(sessionToken)
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
	// Other captured headers (UA, Accept, …) travel as xhdr so the proxy
	// replays them upstream.
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
	// vpnParam pins every proxied URL to the plugin's egress profile
	// ("&vpn=1&egr=<name>"), empty when its video flow goes direct.
	vpnParam := ""
	if egr := engine.LuaPlugins.PluginEgress(pluginID); egr != "" && egr != managers.EgressDirect {
		vpnParam = "&vpn=1&egr=" + url.QueryEscape(egr)
	}

	if isHLS {
		// A sid lets the proxy re-resolve when the upstream rejects the token (live
		// or VOD).
		sidParam := ""
		if pluginID != "" && streamID != "" {
			sid := b64(pluginID + "\x00" + streamID)
			sidParam = "&sid=" + url.QueryEscape(sid)
		}
		return core.AppendProxySig(fmt.Sprintf("%s://%s/proxy/playlist.m3u8?data=%s&origin=%s&cookies=%s%s%s%s%s",
			proxyScheme, proxyHost, b64(rawURL), b64(origin), b64(cookie), sidParam, uidParam, xhdrParam, vpnParam))
	}
	return core.AppendProxySig(fmt.Sprintf("%s://%s/proxy/segment.ts?data=%s&origin=%s&cookies=%s%s%s%s",
		proxyScheme, proxyHost, b64(rawURL), b64(origin), b64(cookie), uidParam, xhdrParam, vpnParam))
}

func (h *MediaHandler) UpdateProgress(ctx context.Context, req *gen.ProgressRequest) (*gen.ProgressResponse, error) {
	deviceID, _ := ctx.Value(ctxKeyDeviceID{}).(string)
	clientID := deviceID
	if clientID == "" {
		clientID = "unknown"
	}
	movedTo := "" // another device took this profile's playback over
	if pid := profileIDFromCtx(ctx); pid != "" {
		if !profileExists(pid) {
			return nil, status.Error(codes.NotFound, "unknown profile")
		}
		clientID = pid
		// The player's heartbeat keeps the lease alive while paused (no segment
		// traffic) and for direct_stream plugins. A device that lost the lease still
		// saves its progress.
		if holder, ok := managers.PlaybackLeases.Claim(pid, deviceID); !ok {
			movedTo = holder
		}
	}
	err := managers.DB.UpsertProgress(
		clientID, req.PluginId, req.MediaId, req.ParentId,
		req.NavigationContext, req.Title, req.Poster,
		float64(req.CurrentPosition), float64(req.TotalDuration),
		req.Rating, req.Genres, req.Plot, req.Year,
		req.SeasonNumber, req.EpisodeNumber,
	)
	if err != nil {
		// A real error, not Ok=false: clients only look at the RPC outcome.
		log.Printf("[pileus/media] UpdateProgress: %v", err)
		return nil, status.Error(codes.Internal, "salvataggio avanzamento non riuscito")
	}
	resp := &gen.ProgressResponse{Ok: true}
	if movedTo != "" {
		resp.PlaybackElsewhere, resp.PlayingOn = true, DeviceDisplayName(movedTo)
	}
	return resp, nil
}

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
		// A real error, not Ok=false: clients only look at the RPC outcome.
		log.Printf("[pileus/media] DeleteProgress: %v", err)
		return nil, status.Error(codes.Internal, "rimozione da Continua a guardare non riuscita")
	}
	return &gen.DeleteProgressResponse{Ok: true}, nil
}

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

	entries, err := managers.DB.GetContinueWatchingFor(clientID, limit, req.ParentId, req.PluginId)
	if err != nil {
		log.Printf("[pileus/media] GetContinueWatching: %v", err)
		return nil, status.Error(codes.Internal, "errore recupero cronologia")
	}

	items := make([]*gen.ContinueWatchingItem, 0, len(entries))
	for _, e := range entries {
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
			SeasonNumber:      e.SeasonNumber,
			EpisodeNumber:     e.EpisodeNumber,
		})
	}
	return &gen.ContinueWatchingResponse{Items: items}, nil
}

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

	// isKnownEmpty[key] means the last page-1 fetch returned 0 items.
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

		// Advertise search filters when the manifest declares the entrypoint.
		caps := append([]string(nil), mf.Exposes.Capabilities...)
		if mf.Entrypoints[engine.EPGetSearchFilters] != "" {
			caps = append(caps, "search_filters")
		}
		// Offline downloads: opted into by the manifest, and only when the server
		// can run them.
		if mf.Download.Enabled && downloads.M != nil && downloads.M.FFmpegAvailable() {
			caps = append(caps, "download")
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

// buildEmptyCatalogSet returns the CatalogCountKeys whose last page-1 count
// is 0, in one MGET.
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

// playbackInUseError is ResolveStream's refusal when the profile is playing
// on another device. ABORTED is reserved for this, so the client can offer
// "watch here" (take_over).
func playbackInUseError(holderDevice string) error {
	return status.Errorf(codes.Aborted, "questo profilo è già in riproduzione su %s", DeviceDisplayName(holderDevice))
}

// DeviceDisplayName is how a device is named to the user: «label», or a
// generic phrase.
func DeviceDisplayName(deviceID string) string {
	if l := deviceLabel(deviceID); l != "" {
		return "«" + l + "»"
	}
	return "un altro dispositivo"
}

// ReleasePlayback frees this device's hold on the profile's playback (the
// player closed).
func (h *MediaHandler) ReleasePlayback(ctx context.Context, _ *gen.ReleasePlaybackRequest) (*gen.ReleasePlaybackResponse, error) {
	deviceID, _ := ctx.Value(ctxKeyDeviceID{}).(string)
	if pid := profileIDFromCtx(ctx); pid != "" && managers.PlaybackLeases.ReleaseIfHolder(pid, deviceID) {
		log.Printf("[pileus/media] profilo %s: riproduzione rilasciata da %s", pid, deviceID)
	}
	return &gen.ReleasePlaybackResponse{Ok: true}, nil
}

func profileIDFromCtx(ctx context.Context) string {
	pid, _ := ctx.Value(ctxKeyProfileID{}).(string)
	return pid
}

// wrapInternal turns a backend error into a gRPC status the player can act
// on: gRPC statuses pass through; known errors map to a user-facing message
// and a code that says whether retrying makes sense.
func wrapInternal(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	var pe *engine.PluginError
	switch {
	case errors.As(err, &pe):
		// The plugin described the failure itself: pass its words through.
		return status.Error(codes.FailedPrecondition, pe.Msg)
	case errors.Is(err, engine.ErrPluginTimeout):
		return status.Error(codes.DeadlineExceeded, "La sorgente non ha risposto in tempo, riprova")
	case errors.Is(err, engine.ErrPluginBusy):
		return status.Error(codes.Unavailable, "Sorgente occupata, riprova tra poco")
	case errors.Is(err, engine.ErrPluginNotLoaded):
		return status.Error(codes.NotFound, "Plugin non disponibile")
	}
	return status.Error(codes.Internal, err.Error())
}

var aniSkipClient = &http.Client{Timeout: 6 * time.Second}

// canonicalHeaderKeys returns h with canonical keys ("cookie" → "Cookie").
// On a collision a non-empty value wins. nil for a nil map.
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
