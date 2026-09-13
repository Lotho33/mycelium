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
	pluginName, err = sanitizeName(pluginName)
	if err != nil {
		http.Error(w, "nome file non valido", http.StatusBadRequest)
		return
	}
	// Belt-and-braces on top of sanitizeName: never let an empty, ".", or
	// ".." name reach the path below, and make sure the resulting directory
	// is a direct child of plugins/ — not plugins/ itself and not something
	// one level up or down from it. This is what actually stopped the
	// "....zip" bug: sanitizeName("..") == "" before this fix, and
	// AppPath("plugins", "") resolves to plugins/ itself, so every file in
	// the archive silently overwrote whatever plugin already lived there.
	if pluginName == "" || pluginName == "." || pluginName == ".." {
		http.Error(w, "nome plugin non valido", http.StatusBadRequest)
		return
	}
	destDir := core.AppPath("plugins", pluginName)
	pluginsRoot := filepath.Clean(core.AppPath("plugins"))
	destDirClean := filepath.Clean(destDir)
	if destDirClean == pluginsRoot || filepath.Dir(destDirClean) != pluginsRoot {
		http.Error(w, "nome plugin non valido", http.StatusBadRequest)
		return
	}

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

	// maxTotalExtractedSize limita la somma su tutto l'archivio — il cap
	// per-file (10 MiB) da solo non impedisce a uno zip con molti file
	// piccoli di riempire comunque il disco. Logica di estrazione (zip-slip,
	// prefisso comune, cap) condivisa con l'updater dell'app web Pileus — vedi
	// zip_extract.go.
	const maxTotalExtractedSize = 200 << 20 // 200 MiB
	aborted, _ := extractZipSafe(&zr.Reader, destDir, 10<<20, maxTotalExtractedSize, "[lua/upload]")

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
