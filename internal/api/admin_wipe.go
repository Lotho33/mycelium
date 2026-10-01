package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"mycelium/internal/core"
	"mycelium/internal/managers"
	"mycelium/internal/pileus"
)

// Destructive dashboard actions: they require the admin password again in
// the body, on top of the session cookie.

func requireAdminPassword(w http.ResponseWriter, r *http.Request) bool {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		http.Error(w, "password richiesta", http.StatusBadRequest)
		return false
	}
	if err := core.VerifyAdmin(body.Password, managers.Settings.MasterAdminHash()); err != nil {
		http.Error(w, "password errata", http.StatusUnauthorized)
		return false
	}
	return true
}

// wipeProfilesHandler deletes every Pileus profile with its watch history
// and per-profile plugin secrets; paired devices stay. Body:
// {"password": "…"}.
func wipeProfilesHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdminPassword(w, r) {
		return
	}

	profRes, err := managers.DB.Exec(`DELETE FROM pileus_profiles`)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Profile PIN trusts go with the profiles (one-off sessions live in memory).
	if _, err := managers.DB.Exec(`DELETE FROM pileus_profile_trust`); err != nil {
		log.Printf("[admin] wipe profiles: profile trust: %v", err)
	}
	pileus.ResetAllProfileAccess()
	histRes, err := managers.DB.Exec(`DELETE FROM watch_history`)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	nProf, _ := profRes.RowsAffected()
	nHist, _ := histRes.RowsAffected()

	if _, err := managers.DB.DeleteAllPluginSecrets(); err != nil {
		log.Printf("[admin] wipe profiles: plugin secrets: %v", err)
	}
	var nRedis int
	if managers.Redis != nil {
		nRedis, _ = managers.Redis.DelByPattern(r.Context(), "mycelium:plugin:*:user:*:secrets")
	}

	log.Printf("[admin] wipe profiles: %d profiles, %d history rows, %d per-profile redis keys", nProf, nHist, nRedis)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"profiles":     nProf,
		"history_rows": nHist,
		"redis_keys":   nRedis,
	})
}

// wipePluginDataHandler flushes Redis and clears the WebP image cache and
// the plugins' on-disk caches; plugins stay installed and configured. Body:
// {"password": "…"}.
func wipePluginDataHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdminPassword(w, r) {
		return
	}

	var redisErr string
	if managers.Redis != nil {
		if err := managers.Redis.FlushDB(r.Context()); err != nil {
			redisErr = err.Error()
		}
	}

	imgN := clearDirContents(core.AppPath("data", "imgcache"))
	fileN := clearPluginCacheFiles()
	managers.Cache.Flush() // in-memory browse/search cache is plugin-derived too

	log.Printf("[admin] wipe plugin data: redis flush err=%q, %d webp files, %d plugin cache files", redisErr, imgN, fileN)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":                 redisErr == "",
		"redis_error":        redisErr,
		"webp_files":         imgN,
		"plugin_cache_files": fileN,
	})
}

// clearDirContents deletes every regular file directly inside dir (the
// directory stays). Dotfiles are kept (placeholders like .gitkeep). A
// missing dir gives 0.
func clearDirContents(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// pluginCacheFiles lists a plugin directory's regenerated runtime files, by
// naming convention (*_cache.json, *_index.json): never source or
// configuration.
func pluginCacheFiles(dir string) []string {
	var out []string
	for _, pat := range []string{"*_cache.json", "*_index.json"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		out = append(out, m...)
	}
	return out
}

// clearPluginCacheFiles removes the regenerated cache files (pluginCacheFiles)
// from every plugins/<id>/ directory. Plugins repopulate them on the next sync.
func clearPluginCacheFiles() int {
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
		for _, f := range pluginCacheFiles(filepath.Join(root, e.Name())) {
			if os.Remove(f) == nil {
				n++
			}
		}
	}
	return n
}
