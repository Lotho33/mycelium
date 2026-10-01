// Per-plugin data wipe: one plugin's Redis cache and on-disk cache files
// (e.g. to force a full re-sync), leaving every other plugin alone.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// wipePluginDataFor clears one plugin's Redis cache and on-disk cache files
// (not its settings or secrets). Body: {"password": "…"}.
func wipePluginDataFor(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	// Reject traversal attempts on top of requiring a loaded plugin.
	if pluginID == "" || filepath.Base(pluginID) != pluginID || !engine.LuaPlugins.Has(pluginID) {
		http.Error(w, "plugin non trovato", http.StatusNotFound)
		return
	}
	if !requireAdminPassword(w, r) {
		return
	}

	var redisN int
	var redisErr string
	if managers.Redis != nil {
		// mycelium.cache.* keys all live under "mycelium:plugin:<id>:cache:",
		// whatever key the plugin passes, so this pattern covers exactly this
		// plugin's cache. Secrets are configuration, not cache: untouched.
		n, err := managers.Redis.DelByPattern(r.Context(), "mycelium:plugin:"+pluginID+":cache:*")
		redisN = n
		if err != nil {
			redisErr = err.Error()
		}
	}

	fileN := clearPluginCacheFilesFor(pluginID)

	log.Printf("[admin] wipe plugin data (%s): redis keys=%d err=%q, %d cache files", pluginID, redisN, redisErr, fileN)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":          redisErr == "",
		"plugin_id":   pluginID,
		"redis_error": redisErr,
		"redis_keys":  redisN,
		"cache_files": fileN,
	})
}

// clearPluginCacheFilesFor is clearPluginCacheFiles for one plugin's
// directory.
func clearPluginCacheFilesFor(pluginID string) int {
	n := 0
	for _, f := range pluginCacheFiles(core.AppPath("plugins", pluginID)) {
		if os.Remove(f) == nil {
			n++
		}
	}
	return n
}
