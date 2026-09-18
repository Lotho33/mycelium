// Per-plugin data wipe — the scoped counterpart to wipePluginDataHandler
// (admin_wipe.go), which is all-or-nothing (FlushDB + every plugin's cache
// files). Requested explicitly: clearing one plugin's scraped/cached data
// (e.g. to force a full re-sync after an update) shouldn't have to blow away
// every other plugin's Redis-backed cache and on-disk cache files too.
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

// wipePluginDataFor godoc
//
//	@Summary		Cancella i dati di un singolo plugin
//	@Description	DANGER: cancella la cache Redis e i file cache su disco di UN plugin (non i secret/impostazioni, non gli altri plugin). Richiede la password admin nel body.
//	@Tags			Admin
//	@Accept			json
//	@Param			plugin_id	path	string					true	"id del plugin"
//	@Param			body		body	object{password=string}	true	"password admin"
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/lua-plugins/wipe-data/{plugin_id} [post]
func wipePluginDataFor(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	// filepath.Base(pluginID) != pluginID would reject a traversal attempt
	// (e.g. "../other") on top of requiring the id to actually be a
	// currently-loaded plugin — belt and braces, same spirit as the
	// zip-upload path-safety checks.
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
		// mycelium.cache.* (lua_sdk.go, buildCacheModule) namespaces every
		// key a plugin writes under "mycelium:plugin:<id>:cache:" regardless
		// of what key string the plugin's own Lua code passes in — so this
		// one pattern reliably covers all of a plugin's cache.set/get/del
		// traffic, never spilling into another plugin's keys and never
		// missing some of this plugin's own keys because of how its author
		// happened to name them internally (e.g. animeunity's own
		// "animeunity:..."-prefixed key strings still land under this
		// engine-enforced prefix). Deliberately NOT touched:
		// managers.SecretsKey(pluginID, ...) — admin-entered credentials
		// (API tokens, domains) are configuration, not cache, and clearing
		// them here would be a surprising side effect of a "clear data"
		// button.
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

// clearPluginCacheFilesFor removes the same known runtime cache filenames as
// clearPluginCacheFiles (admin_wipe.go) but from a single plugin's directory
// instead of sweeping every plugin directory.
func clearPluginCacheFilesFor(pluginID string) int {
	names := []string{"catalog_cache.json", "logo_cache.json", "fribb_index.json", "avail_cache.json"}
	dir := core.AppPath("plugins", pluginID)
	n := 0
	for _, name := range names {
		if os.Remove(filepath.Join(dir, name)) == nil {
			n++
		}
	}
	return n
}
