package pileus

import (
	"sync"
	"time"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// Shared live links. For a plugin with share_live, a Play of a live stream
// someone is already watching reuses the link resolved for them instead of
// asking the site for a new one: every player then reads the same upstream
// playlist, which the proxy fetches once for all (api/proxy_live.go).

// liveShareIdle: the shared link is dropped when no player of the stream
// fetched anything for this long.
const liveShareIdle = 45 * time.Second

type liveShare struct {
	pluginID, streamID string
	rs                 *ResolvedStream
}

var liveShares = struct {
	sync.Mutex
	m map[string]liveShare
}{m: map[string]liveShare{}}

// shareLiveEnabled is a seam for tests.
var shareLiveEnabled = engine.LuaPlugins.ShareLiveEnabled

// liveShared returns the link to reuse for pluginID/streamID, or nil.
func liveShared(pluginID, streamID string) *ResolvedStream {
	if !shareLiveEnabled(pluginID) {
		return nil
	}
	key := resolveFlightKey(pluginID, streamID)
	liveShares.Lock()
	e, ok := liveShares.m[key]
	liveShares.Unlock()
	if !ok {
		return nil
	}
	if !managers.Sessions.LiveWatched(pluginID, streamID, liveShareIdle) {
		liveShares.Lock()
		if cur, ok := liveShares.m[key]; ok && cur.rs == e.rs {
			delete(liveShares.m, key)
		}
		liveShares.Unlock()
		return nil
	}
	return e.rs
}

// liveShareStore remembers a freshly resolved live link for the next Plays,
// and drops those of streams nobody watches any more.
func liveShareStore(pluginID, streamID string, rs *ResolvedStream) {
	if rs == nil || !rs.IsLive || !shareLiveEnabled(pluginID) {
		return
	}
	key := resolveFlightKey(pluginID, streamID)
	liveShares.Lock()
	defer liveShares.Unlock()
	liveShares.m[key] = liveShare{pluginID: pluginID, streamID: streamID, rs: rs}
	for k, e := range liveShares.m {
		if k != key && !managers.Sessions.LiveWatched(e.pluginID, e.streamID, liveShareIdle) {
			delete(liveShares.m, k)
		}
	}
}

// RefreshLiveShare replaces the URL and headers of pluginID/streamID's shared
// live link, if there is one, with what the proxy just re-resolved (an
// expired token renewed mid-stream): the next Play gets a link that works
// instead of the one handed out when the stream started.
func RefreshLiveShare(pluginID, streamID, url string, headers map[string]string) {
	key := resolveFlightKey(pluginID, streamID)
	liveShares.Lock()
	defer liveShares.Unlock()
	e, ok := liveShares.m[key]
	if !ok || e.rs == nil || url == "" {
		return
	}
	fresh := *e.rs
	fresh.URL, fresh.Headers = url, headers
	e.rs = &fresh
	liveShares.m[key] = e
}
