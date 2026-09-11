package api

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ─── file editor ──────────────────────────────────────────────────────────────

// listLuaPluginFiles returns all .lua and .yaml files inside a plugin directory.
func listLuaPluginFiles(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	pluginDir, err := findLuaPluginDir(pluginID)
	if err != nil {
		http.Error(w, "plugin non trovato", http.StatusNotFound)
		return
	}

	type fileEntry struct {
		Path string `json:"path"` // relative to plugin dir
		Size int64  `json:"size"`
	}
	var files []fileEntry
	err = filepath.WalkDir(pluginDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(pluginDir, path)
		rel = filepath.ToSlash(rel)
		ext := strings.ToLower(filepath.Ext(rel))
		if ext != ".lua" && ext != ".yaml" && ext != ".yml" && ext != ".json" {
			return nil
		}
		info, _ := d.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		files = append(files, fileEntry{Path: rel, Size: size})
		return nil
	})
	if err != nil {
		http.Error(w, "errore lettura directory", http.StatusInternalServerError)
		return
	}
	if files == nil {
		files = []fileEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

// getLuaPluginFile returns the content of a single file within a plugin directory.
// Query param: ?path=init.lua (relative path, sanitized)
func getLuaPluginFile(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		relPath = "init.lua"
	}

	pluginDir, err := findLuaPluginDir(pluginID)
	if err != nil {
		http.Error(w, "plugin non trovato", http.StatusNotFound)
		return
	}

	abs, ok := safeJoin(pluginDir, relPath)
	if !ok {
		http.Error(w, "path non valido", http.StatusBadRequest)
		return
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		http.Error(w, "file non trovato", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"path":    relPath,
		"content": string(data),
	})
}

// putLuaPluginFile writes content to a file within a plugin directory.
// Body: {"content": "..."}
// The hot-reload watcher picks up the mtime change within 2 seconds.
func putLuaPluginFile(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		relPath = "init.lua"
	}

	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "JSON non valido", http.StatusBadRequest)
		return
	}

	pluginDir, err := findLuaPluginDir(pluginID)
	if err != nil {
		http.Error(w, "plugin non trovato", http.StatusNotFound)
		return
	}

	abs, ok := safeJoin(pluginDir, relPath)
	if !ok {
		http.Error(w, "path non valido", http.StatusBadRequest)
		return
	}

	if err := os.WriteFile(abs, []byte(body.Content), 0644); err != nil {
		http.Error(w, "errore scrittura file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("[lua/editor] file salvato: %s/%s", pluginID, relPath)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
