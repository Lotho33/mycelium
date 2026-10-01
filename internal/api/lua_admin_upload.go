package api

import (
	"archive/zip"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"mycelium/internal/core"
	"mycelium/internal/engine"
)

// ─── upload (ZIP with manifest.yaml + *.lua) ──────────────────────────────────

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

	// Extract to a staging directory first: the destination is derived from the
	// id declared in manifest.yaml, not from the ZIP's name, so uploading a new
	// version updates the installed plugin.
	stagingDir, err := os.MkdirTemp(pluginsRoot, ".staging-*")
	if err != nil {
		http.Error(w, "errore creazione staging", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(stagingDir) // no-op once renamed into place below

	// maxTotalExtractedSize caps the whole archive (the per-file cap alone
	// doesn't stop many small files). Extraction is shared with the web-app
	// updater (zip_extract.go).
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
	// readLuaManifest already validated mf.ID against the id whitelist: use it
	// as the directory name.
	pluginName := mf.ID
	destDir := core.AppPath("plugins", pluginName)
	destDirClean := filepath.Clean(destDir)
	if destDirClean == pluginsRoot || filepath.Dir(destDirClean) != pluginsRoot {
		http.Error(w, "id plugin non valido", http.StatusBadRequest)
		return
	}

	// "shared" is not a plugin: its modules are preloaded into every plugin, so
	// an upload with that id would run code under other plugins' identity.
	if pluginName == "shared" {
		http.Error(w, "id plugin riservato: shared", http.StatusBadRequest)
		return
	}
	isUpdate := engine.LuaPlugins.Has(pluginName)
	// An existing directory that isn't this plugin is not ours to merge into.
	if !isUpdate {
		if _, err := os.Stat(destDir); err == nil {
			if existing, merr := readLuaManifest(destDir); merr != nil || existing.ID != pluginName {
				http.Error(w, "la cartella plugins/"+pluginName+" esiste già e non contiene questo plugin", http.StatusConflict)
				return
			}
			isUpdate = true // installed but not loaded
		}
	}
	if isUpdate {
		// Hot-swap: unload the old pool before its directory changes. Files are
		// merged, never wiped: runtime data (*_cache.json, *_index.json) survives
		// unless the ZIP itself contains such files.
		engine.LuaPlugins.UnloadPlugin(pluginName)
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		http.Error(w, "errore creazione directory", http.StatusInternalServerError)
		return
	}
	if isUpdate {
		// Code comes only from the new ZIP: drop the old .lua modules first so a
		// removed one doesn't stay require-able.
		if err := removeLuaFiles(destDir); err != nil {
			http.Error(w, "errore rimozione moduli precedenti: "+err.Error(), http.StatusInternalServerError)
			return
		}
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

// removeLuaFiles deletes every *.lua file under dir (recursively), leaving
// all other files and the directory tree in place.
func removeLuaFiles(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".lua") {
			return os.Remove(path)
		}
		return nil
	})
}

// mergeDirInto moves every entry of src (a staging directory) into dst,
// overwriting same-named entries and leaving the others untouched. src is
// consumed.
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
		// Same filesystem, so Rename is a move; copy+remove as a fallback.
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
