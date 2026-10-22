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

// editorMaxFileBytes caps a file the editor opens (runtime caches can be
// megabytes of JSON nobody edits by hand).
const editorMaxFileBytes = 4 << 20

// editorEditable reports whether rel has an extension the editor handles.
func editorEditable(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".lua", ".yaml", ".yml", ".json":
		return true
	}
	return false
}

// editorPath resolves rel inside pluginDir for the editor: an editable
// extension, lexically inside the directory, and no symlink on the way out
// of it (the file itself or a parent directory).
func editorPath(pluginDir, rel string) (string, bool) {
	if !editorEditable(rel) {
		return "", false
	}
	abs, ok := safeJoin(pluginDir, rel)
	if !ok {
		return "", false
	}
	root, err := filepath.EvalSymlinks(pluginDir)
	if err != nil {
		return "", false
	}
	if fi, err := os.Lstat(abs); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", false
	}
	if parent != root && !strings.HasPrefix(parent, root+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}

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
		if !editorEditable(rel) || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, _ := d.Info()
		if info == nil || info.Size() > editorMaxFileBytes {
			return nil
		}
		files = append(files, fileEntry{Path: rel, Size: info.Size()})
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

	abs, ok := editorPath(pluginDir, relPath)
	if !ok {
		http.Error(w, "path non valido", http.StatusBadRequest)
		return
	}
	if fi, err := os.Stat(abs); err == nil && fi.Size() > editorMaxFileBytes {
		http.Error(w, "file troppo grande per l'editor", http.StatusRequestEntityTooLarge)
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

	abs, ok := editorPath(pluginDir, relPath)
	if !ok {
		http.Error(w, "path non valido", http.StatusBadRequest)
		return
	}

	// tmp + rename: the hot reload never reads a half-written file.
	tmp := abs + ".tmp"
	if err := os.WriteFile(tmp, []byte(body.Content), 0o644); err != nil {
		http.Error(w, "errore scrittura file", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		http.Error(w, "errore scrittura file", http.StatusInternalServerError)
		return
	}

	log.Printf("[lua/editor] file salvato: %s/%s", pluginID, relPath)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
