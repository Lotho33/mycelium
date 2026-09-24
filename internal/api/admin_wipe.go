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
)

// Destructive dashboard actions — both require the admin password again in
// the request body, on top of the session cookie, since a stray click here
// isn't recoverable.

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

// wipeProfilesHandler godoc
//
//	@Summary		Cancella tutti i profili
//	@Description	DANGER: elimina tutti i profili Pileus, la loro cronologia di visione e i secret per-profilo dei plugin. I device accoppiati restano. Richiede la password admin nel body.
//	@Tags			Admin
//	@Accept			json
//	@Param			body	body	object{password=string}	true	"password admin"
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/profiles/wipe [post]
func wipeProfilesHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdminPassword(w, r) {
		return
	}

	profRes, err := managers.DB.Exec(`DELETE FROM pileus_profiles`)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
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

// wipePluginDataHandler godoc
//
//	@Summary		Cancella tutti i dati dei plugin
//	@Description	DANGER: FLUSHDB su Redis, svuota la cache immagini WebP e i file cache su disco dei plugin. I plugin restano installati, abilitati e configurati. Richiede la password admin nel body.
//	@Tags			Admin
//	@Accept			json
//	@Param			body	body	object{password=string}	true	"password admin"
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/plugins/wipe-data [post]
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
// directory itself is kept). Dotfiles are left alone — they're placeholders/
// metadata, never actual plugin/cache content: plugins/.gitkeep in
// particular only exists so git (and so the Docker build, which COPYs
// plugins/) has a non-empty directory to track when no plugin is bundled —
// deleting it here doesn't just lose a marker, a later commit of the
// now-empty plugins/ breaks the next image build (git tracks no files under
// an empty dir at all, so `COPY plugins/` fails outright — this happened for
// real once already). Missing dir → 0, no error.
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

// clearPluginCacheFiles removes the known regenerated cache files from every
// plugins/<id>/ directory — the same set kept out of the image by
// .dockerignore. Plugins repopulate them on the next sync.
func clearPluginCacheFiles() int {
	names := []string{"catalog_cache.json", "logo_cache.json", "fribb_index.json", "avail_cache.json"}
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
		for _, name := range names {
			if os.Remove(filepath.Join(root, e.Name(), name)) == nil {
				n++
			}
		}
	}
	return n
}
