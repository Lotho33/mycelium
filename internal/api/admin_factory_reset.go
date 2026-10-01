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
	"mycelium/internal/pileus"
)

// factoryResetHandler deletes everything — profiles, paired devices, plugin
// data and caches, the plugins themselves and every setting, the admin
// password included — so the next visit starts the setup over.
// Irreversible. Body: {"password": "…"}.
func factoryResetHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdminPassword(w, r) {
		return
	}

	profRes, err := managers.DB.Exec(`DELETE FROM pileus_profiles`)
	if err != nil {
		http.Error(w, "db error (profiles): "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Profile PIN trusts go with the profiles (one-off sessions live in memory).
	if _, err := managers.DB.Exec(`DELETE FROM pileus_profile_trust`); err != nil {
		log.Printf("[admin] factory reset: profile trust: %v", err)
	}
	pileus.ResetAllProfileAccess()
	histRes, err := managers.DB.Exec(`DELETE FROM watch_history`)
	if err != nil {
		http.Error(w, "db error (watch_history): "+err.Error(), http.StatusInternalServerError)
		return
	}
	devRes, err := managers.DB.Exec(`DELETE FROM pileus_devices`)
	if err != nil {
		http.Error(w, "db error (devices): "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := managers.DB.DeleteAllPluginSecrets(); err != nil {
		http.Error(w, "db error (plugin secrets): "+err.Error(), http.StatusInternalServerError)
		return
	}
	nProf, _ := profRes.RowsAffected()
	nHist, _ := histRes.RowsAffected()
	nDev, _ := devRes.RowsAffected()

	var redisErr string
	if managers.Redis != nil {
		if err := managers.Redis.FlushDB(r.Context()); err != nil {
			redisErr = err.Error()
		}
	}
	imgN := clearDirContents(core.AppPath("data", "imgcache"))
	managers.Cache.Flush()

	// Unload every running Lua plugin before touching its files on disk —
	// same order uninstallLuaPlugin uses for one plugin, just looped over
	// all of them (see internal/api/lua_admin_upload.go).
	metas := engine.LuaPlugins.GetMeta()
	for _, m := range metas {
		engine.LuaPlugins.UnloadPlugin(m.ID)
	}
	pluginN := clearDirContents(core.AppPath("plugins")) + clearPluginSubdirs()

	if err := managers.Settings.ResetAll(); err != nil {
		http.Error(w, "settings reset error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The current browser's session cookie is now pointing at a wiped
	// identity — revoke it and clear it so the next request (the frontend
	// redirects to /setup right after this response) doesn't land on an
	// authenticated-but-nonsensical dashboard. Sessions elsewhere (another
	// device, another tab) stay valid until their own 12h expiry: revoking
	// those too would mean rotating the session signing key itself, out of
	// scope for what a stray click here needs to guard against.
	if c, err := r.Cookie(adminSessionCookie); err == nil {
		revokeAdminSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   adminSessionCookie,
		Value:  "",
		Path:   "/admin",
		MaxAge: -1,
	})

	log.Printf("[admin] FACTORY RESET: %d profiles, %d history rows, %d devices, %d plugins unloaded (%d dirs removed), %d webp files, redis flush err=%q",
		nProf, nHist, nDev, len(metas), pluginN, imgN, redisErr)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":           redisErr == "",
		"redis_error":  redisErr,
		"profiles":     nProf,
		"history_rows": nHist,
		"devices":      nDev,
		"plugins":      len(metas),
		"plugin_dirs":  pluginN,
		"webp_files":   imgN,
	})
}

// clearPluginSubdirs removes every entry directly inside plugins/ that
// clearDirContents left behind (it only deletes files, not directories) —
// each plugin lives in its own subdirectory. Missing dir → 0, no error.
func clearPluginSubdirs() int {
	root := core.AppPath("plugins")
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if os.RemoveAll(filepath.Join(root, e.Name())) == nil {
			n++
		}
	}
	return n
}
