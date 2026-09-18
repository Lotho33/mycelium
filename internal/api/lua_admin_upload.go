package api

import (
	"archive/zip"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"mycelium/internal/core"
	"mycelium/internal/engine"
)

// ─── upload (ZIP con manifest.yaml + *.lua) ───────────────────────────────────

func uploadLuaPlugin(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(50 << 20)
	file, header, err := r.FormFile("plugin_file")
	if err != nil {
		http.Error(w, "nessun file ricevuto", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".zip") {
		http.Error(w, "solo .zip supportati per i plugin Lua", http.StatusBadRequest)
		return
	}

	pluginsRoot := filepath.Clean(core.AppPath("plugins"))

	// Write ZIP to temp file so zip.OpenReader can seek.
	tmp, err := os.CreateTemp("", "lua_upload_*.zip")
	if err != nil {
		http.Error(w, "errore file temporaneo", http.StatusInternalServerError)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, io.LimitReader(file, 50<<20)); err != nil {
		tmp.Close()
		http.Error(w, "errore scrittura", http.StatusInternalServerError)
		return
	}
	tmp.Close()

	zr, err := zip.OpenReader(tmpPath)
	if err != nil {
		http.Error(w, "ZIP corrotto", http.StatusBadRequest)
		return
	}
	defer zr.Close()

	// Validate: must contain manifest.yaml and init.lua (anywhere inside the zip).
	hasManifest, hasInit := false, false
	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		if base == "manifest.yaml" {
			hasManifest = true
		}
		if base == "init.lua" {
			hasInit = true
		}
	}
	if !hasManifest || !hasInit {
		http.Error(w, "ZIP deve contenere manifest.yaml e init.lua", http.StatusBadRequest)
		return
	}

	// Extract to a staging directory first — the final destination directory
	// is derived from the *declared id in manifest.yaml*, not from the ZIP's
	// filename. This is what makes "upload a ZIP to update an existing
	// plugin" actually reliable: before this, destDir came straight from
	// header.Filename (see git history), so re-uploading an updated
	// "animeunity_v2.zip" for the already-installed "animeunity" plugin would
	// have created a *second*, separate plugin instead of updating the first
	// — silently, with no error. Staging first means the real id is known
	// before any existing installation is touched.
	stagingDir, err := os.MkdirTemp(pluginsRoot, ".staging-*")
	if err != nil {
		http.Error(w, "errore creazione staging", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(stagingDir) // no-op once renamed into place below

	// maxTotalExtractedSize limita la somma su tutto l'archivio — il cap
	// per-file (10 MiB) da solo non impedisce a uno zip con molti file
	// piccoli di riempire comunque il disco. Logica di estrazione (zip-slip,
	// prefisso comune, cap) condivisa con l'updater dell'app web Pileus — vedi
	// zip_extract.go.
	const maxTotalExtractedSize = 200 << 20 // 200 MiB
	aborted, _ := extractZipSafe(&zr.Reader, stagingDir, 10<<20, maxTotalExtractedSize, "[lua/upload]")
	if aborted {
		http.Error(w, "archivio troppo grande una volta estratto", http.StatusBadRequest)
		return
	}

	mf, err := readLuaManifest(stagingDir)
	if err != nil {
		http.Error(w, "manifest.yaml non valido: "+err.Error(), http.StatusBadRequest)
		return
	}
	// readLuaManifest already validates mf.ID against the same charset
	// whitelist used to close the XSS gap in the plugin-id-in-dashboard fix
	// (^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$) — no need to re-derive/sanitize
	// it here, just use it as the single source of truth for the directory
	// name (sanitizeName is still used elsewhere, e.g. for editor filenames).
	pluginName := mf.ID
	destDir := core.AppPath("plugins", pluginName)
	destDirClean := filepath.Clean(destDir)
	if destDirClean == pluginsRoot || filepath.Dir(destDirClean) != pluginsRoot {
		http.Error(w, "id plugin non valido", http.StatusBadRequest)
		return
	}

	isUpdate := engine.LuaPlugins.Has(pluginName)
	if isUpdate {
		// Safe hot-swap: stop the old pool/watchers before the directory
		// underneath them changes. UnloadPlugin never deletes files — only
		// the engine's in-memory registration — so anything the old plugin
		// wrote at runtime (Redis-backed cache, on-disk JSON caches such as
		// catalog_cache.json/fribb_index.json) survives untouched: the merge
		// below only overwrites files that are actually present in the new
		// ZIP, it never wipes destDir first. A plugin update ZIP built
		// without bundling those runtime cache files keeps them intact
		// across the update — building one WITH stale cache files bundled
		// would instead overwrite live data with whatever was in the ZIP.
		engine.LuaPlugins.UnloadPlugin(pluginName)
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		http.Error(w, "errore creazione directory", http.StatusInternalServerError)
		return
	}
	if err := mergeDirInto(stagingDir, destDir); err != nil {
		http.Error(w, "errore installazione file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := engine.LuaPlugins.LoadPlugin(destDir); err != nil {
		http.Error(w, "plugin estratto ma caricamento fallito: "+err.Error(), http.StatusInternalServerError)
		return
	}

	verb := "installato"
	if isUpdate {
		verb = "aggiornato"
	}
	log.Printf("[lua/upload] plugin %s: %s", verb, pluginName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"ok":      "true",
		"message": "Plugin Lua " + verb + " e caricato.",
		"id":      pluginName,
		"update":  boolToString(isUpdate),
	})
}

func boolToString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// mergeDirInto moves every entry from src (a staging extraction directory)
// into dst, overwriting same-named files/directories in dst but leaving any
// pre-existing dst entry that src doesn't have completely untouched — the
// property an update relies on to preserve runtime cache files silently
// (see the comment above the isUpdate branch in uploadLuaPlugin). src is
// consumed (entries are renamed away, not copied) since it's a throwaway
// staging directory the caller removes afterward regardless.
func mergeDirInto(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(to, 0755); err != nil {
				return err
			}
			if err := mergeDirInto(from, to); err != nil {
				return err
			}
			continue
		}
		// Same filesystem (both under plugins/), so Rename is an atomic
		// same-volume move — but fall back to a copy+remove if it ever isn't
		// (e.g. a future deployment mounts plugins/ across two volumes).
		if err := os.Rename(from, to); err != nil {
			data, rerr := os.ReadFile(from)
			if rerr != nil {
				return rerr
			}
			if werr := os.WriteFile(to, data, 0644); werr != nil {
				return werr
			}
		}
	}
	return nil
}

// ─── uninstall ────────────────────────────────────────────────────────────────

func uninstallLuaPlugin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PluginID string `json:"plugin_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PluginID == "" {
		http.Error(w, "plugin_id mancante", http.StatusBadRequest)
		return
	}

	engine.LuaPlugins.UnloadPlugin(body.PluginID)

	// Find and delete the plugin directory.
	entries, err := os.ReadDir(core.AppPath("plugins"))
	if err != nil {
		http.Error(w, "errore lettura plugins dir", http.StatusInternalServerError)
		return
	}

	removed := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pluginDir := core.AppPath("plugins", entry.Name())
		mf, err := readLuaManifest(pluginDir)
		if err != nil || mf.ID != body.PluginID {
			continue
		}
		if err := os.RemoveAll(pluginDir); err != nil {
			http.Error(w, "errore rimozione file: "+err.Error(), http.StatusInternalServerError)
			return
		}
		removed = true
		log.Printf("[lua/uninstall] plugin rimosso: %s", body.PluginID)
		break
	}

	if !removed {
		http.Error(w, "plugin non trovato", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
