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

	pluginName := strings.TrimSuffix(filepath.Base(header.Filename), ".zip")
	pluginName = sanitizeName(pluginName)
	destDir := core.AppPath("plugins", pluginName)

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

	// If already loaded, unload first (safe hot-swap).
	// We'll discover the ID after extraction; for now just remove the old dir if same name.
	if engine.LuaPlugins.Has(pluginName) {
		engine.LuaPlugins.UnloadPlugin(pluginName)
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		http.Error(w, "errore creazione directory", http.StatusInternalServerError)
		return
	}

	extractAbs := filepath.Clean(destDir) + string(os.PathSeparator)

	// Detect a single top-level folder wrapping every entry (common when the
	// archive was made by compressing the plugin directory itself). Only strip
	// it when EVERY non-empty entry shares that first path segment — otherwise
	// leave paths as-is.
	stripPrefix := ""
	for _, f := range zr.File {
		name := strings.TrimPrefix(filepath.ToSlash(f.Name), "./")
		if name == "" || name == "/" {
			continue
		}
		j := strings.IndexByte(name, '/')
		if j < 0 {
			// a file sits at the archive root → no common wrapper folder
			stripPrefix = ""
			break
		}
		seg := name[:j+1] // "<dir>/"
		if stripPrefix == "" {
			stripPrefix = seg
		} else if stripPrefix != seg {
			stripPrefix = ""
			break
		}
	}

	// maxTotalExtractedSize limita la somma su tutto l'archivio — il cap
	// per-file (10 MiB) da solo non impedisce a uno zip con molti file
	// piccoli di riempire comunque il disco.
	const maxTotalExtractedSize = 200 << 20 // 200 MiB
	var totalExtracted int64
	aborted := false

	for _, f := range zr.File {
		if aborted {
			break
		}
		rel := filepath.ToSlash(f.Name)
		rel = strings.TrimPrefix(rel, stripPrefix)
		if rel == "" {
			continue
		}
		fpath := filepath.Join(destDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(filepath.Clean(fpath)+string(os.PathSeparator), extractAbs) {
			log.Printf("[lua/upload] path traversal bloccato: %s", f.Name)
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0755)
			continue
		}
		if totalExtracted+int64(f.UncompressedSize64) > maxTotalExtractedSize {
			log.Printf("[lua/upload] archivio oltre il limite totale (%d MiB), estrazione interrotta", maxTotalExtractedSize>>20)
			aborted = true
			break
		}
		os.MkdirAll(filepath.Dir(fpath), 0755)
		out, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			out.Close()
			continue
		}
		n, _ := io.Copy(out, io.LimitReader(rc, 10<<20))
		totalExtracted += n
		out.Close()
		rc.Close()
	}

	if aborted {
		os.RemoveAll(destDir)
		http.Error(w, "archivio troppo grande una volta estratto", http.StatusBadRequest)
		return
	}

	if err := engine.LuaPlugins.LoadPlugin(destDir); err != nil {
		http.Error(w, "plugin estratto ma caricamento fallito: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("[lua/upload] plugin installato: %s", pluginName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"ok":      "true",
		"message": "Plugin Lua installato e caricato.",
		"id":      pluginName,
	})
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
