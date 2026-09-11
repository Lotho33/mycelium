package api

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// RegisterLuaAdminRoutes registers admin endpoints for Lua plugin management.
func RegisterLuaAdminRoutes(mux *http.ServeMux) {
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/lua-plugins", auth(listLuaPlugins))
	mux.HandleFunc("GET /admin/lua-plugins/settings/{plugin_id}", auth(getLuaPluginSettings))
	mux.HandleFunc("POST /admin/lua-plugins/settings/{plugin_id}", auth(saveLuaPluginSettings))
	mux.HandleFunc("POST /admin/lua-plugins/run-task/{plugin_id}/{task}", auth(runLuaTask))
	mux.HandleFunc("POST /admin/lua-plugins/state/{plugin_id}", auth(setLuaPluginState))
	mux.HandleFunc("POST /admin/lua-plugins/upload", auth(uploadLuaPlugin))
	mux.HandleFunc("DELETE /admin/lua-plugins/uninstall", auth(uninstallLuaPlugin))
	mux.HandleFunc("GET /admin/lua-plugins/logs", auth(getLuaPluginLogs))
	mux.HandleFunc("GET /admin/lua-plugins/logs/stream", auth(getLuaPluginLogsSSE))
	mux.HandleFunc("GET /admin/lua-plugins/stats", auth(getLuaPluginStats))
	mux.HandleFunc("GET /admin/lua-plugins/files/{plugin_id}", auth(listLuaPluginFiles))
	mux.HandleFunc("GET /admin/lua-plugins/file/{plugin_id}", auth(getLuaPluginFile))
	mux.HandleFunc("PUT /admin/lua-plugins/file/{plugin_id}", auth(putLuaPluginFile))
}

// ─── task runner ──────────────────────────────────────────────────────────────

func runLuaTask(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	task := r.PathValue("task")

	// Prima bypassava del tutto bgTaskSem — un click "Esegui" poteva girare
	// in parallelo con QUALSIASI numero di task schedulati pesanti, vanificando
	// il motivo per cui il semaforo esiste. Tentativo NON bloccante: se occupato,
	// risposta immediata (nessuna attesa silenziosa per la durata di un sync
	// altrui) con un errore chiaro invece di far partire comunque il task.
	if !engine.TryAcquireBgTaskSem() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"ok":false,"message":"un altro task è già in corso, riprova tra poco"}`))
		return
	}
	// bgTaskSem da solo (capacità 2) non basta a impedire che questo run
	// manuale parta mentre un tick cron dello STESSO plugin è già a metà —
	// serve anche il lock per-plugin, stesso motivo/stesso pattern di
	// TriggerLiveRefresh (vedi TryAcquirePluginTaskLock in lua_plugin.go).
	releaseTaskLock, gotLock := engine.LuaPlugins.TryAcquirePluginTaskLock(pluginID)
	if !gotLock {
		engine.ReleaseBgTaskSem()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"ok":false,"message":"un task di questo plugin è già in corso, riprova tra poco"}`))
		return
	}

	go func() {
		defer engine.ReleaseBgTaskSem()
		defer releaseTaskLock()
		// Goroutine nuda: un panic qui (non un semplice errore Lua, quello
		// è già un ritorno normale via err) crasherebbe l'intero processo
		// senza questo recover — stesso pattern/stessa causa dei crash
		// visti durante i task schedulati pesanti, vedi il commento su
		// runTasks in lua_plugin.go.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[lua] manual task %s/%s: PANIC recuperato: %v", pluginID, task, r)
			}
		}()
		_, err := engine.LuaPlugins.CallEntrypoint(pluginID, task, nil, "")
		engine.LuaPlugins.RecordTaskResult(pluginID, err == nil)
		if err != nil {
			log.Printf("[lua] manual task %s/%s: %v", pluginID, task, err)
		} else {
			log.Printf("[lua] manual task %s/%s: OK", pluginID, task)
			engine.MarkTaskRan(pluginID, task)
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"message":"task started in background"}`))
}

// ─── lifecycle (start / stop / restart) ──────────────────────────────────────

// setLuaPluginState drives a plugin's operational lifecycle from the dashboard.
// Body: {"action": "start" | "stop" | "restart"}.
//   - start   : allowed only when no required setting is unset
//   - stop    : always allowed
//   - restart : reloads manifest + scripts from disk; only on a running plugin
func setLuaPluginState(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	if !engine.LuaPlugins.Has(pluginID) {
		http.Error(w, "plugin non trovato", http.StatusNotFound)
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "JSON non valido", http.StatusBadRequest)
		return
	}

	switch body.Action {
	case "start":
		if missing := engine.LuaPlugins.MissingRequiredSettings(pluginID); len(missing) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "message": "configurazione incompleta", "missing_required": missing,
			})
			return
		}
		engine.LuaPlugins.SetRunEnabled(pluginID, true)
	case "stop":
		engine.LuaPlugins.SetRunEnabled(pluginID, false)
	case "restart":
		if !engine.LuaPlugins.IsOperational(pluginID) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "message": "riavvio possibile solo su un plugin attivo",
			})
			return
		}
		dir, err := findLuaPluginDir(pluginID)
		if err != nil {
			http.Error(w, "directory plugin non trovata", http.StatusNotFound)
			return
		}
		engine.LuaPlugins.UnloadPlugin(pluginID)
		if err := engine.LuaPlugins.LoadPlugin(dir); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "message": "reload fallito: " + err.Error(),
			})
			return
		}
	default:
		http.Error(w, "azione non valida (start|stop|restart)", http.StatusBadRequest)
		return
	}

	log.Printf("[lua] plugin %s: %s → %s", pluginID, body.Action, engine.LuaPlugins.RunStateOf(pluginID))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "run_state": engine.LuaPlugins.RunStateOf(pluginID),
	})
}

// ─── list ─────────────────────────────────────────────────────────────────────

func listLuaPlugins(w http.ResponseWriter, r *http.Request) {
	metas := engine.LuaPlugins.GetMetaWithStatus()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metas)
}

// ─── settings ────────────────────────────────────────────────────────────────

func getLuaPluginSettings(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	metas := engine.LuaPlugins.GetMeta()

	var fields []engine.LuaSettingField
	for _, m := range metas {
		if m.ID == pluginID {
			fields = m.Settings.Global
			break
		}
	}

	result := make([]map[string]any, 0, len(fields))
	for _, f := range fields {
		val := managers.Settings.GetString("lua:"+pluginID+":global:"+f.ID, "")
		entry := map[string]any{
			"id":       f.ID,
			"label":    f.Label,
			"type":     f.Type,
			"required": f.Required,
		}
		if f.Type == "password" {
			entry["is_set"] = val != ""
		} else {
			entry["value"] = val
		}
		result = append(result, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func saveLuaPluginSettings(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")

	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	metas := engine.LuaPlugins.GetMeta()
	var fields []engine.LuaSettingField
	for _, m := range metas {
		if m.ID == pluginID {
			fields = m.Settings.Global
			break
		}
	}
	allowedKeys := make(map[string]bool, len(fields))
	for _, f := range fields {
		allowedKeys[f.ID] = true
	}

	toSave := make(map[string]any, len(body))
	for key, val := range body {
		if len(allowedKeys) > 0 && !allowedKeys[key] {
			continue // silently skip keys not declared by this plugin's manifest
		}
		toSave["lua:"+pluginID+":global:"+key] = val
	}
	if err := managers.Settings.Save(toSave); err != nil {
		http.Error(w, "save error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

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

// ─── logs ─────────────────────────────────────────────────────────────────────

func getLuaPluginLogs(w http.ResponseWriter, r *http.Request) {
	pluginID := r.URL.Query().Get("plugin_id")
	if pluginID == "" {
		http.Error(w, "plugin_id mancante", http.StatusBadRequest)
		return
	}
	buf := engine.LuaPlugins.GetLogBuffer(pluginID)
	lines := []string{}
	if buf != nil {
		lines = buf.Lines()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"logs": lines})
}

func getLuaPluginLogsSSE(w http.ResponseWriter, r *http.Request) {
	pluginID := r.URL.Query().Get("plugin_id")
	if pluginID == "" {
		http.Error(w, "plugin_id mancante", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	buf := engine.LuaPlugins.GetLogBuffer(pluginID)
	if buf == nil {
		fmt.Fprintf(w, "data: (nessun log per %s)\n\n", pluginID)
		flusher.Flush()
		return
	}

	existing, lastTotal := buf.Snapshot()
	for _, line := range existing {
		fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", " "))
	}
	flusher.Flush()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			lines, total := buf.Snapshot()
			newCount := total - lastTotal
			if newCount > 0 {
				start := max(int64(len(lines))-newCount, 0)
				for _, line := range lines[start:] {
					fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", " "))
				}
			}
			lastTotal = total
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// ─── stats ────────────────────────────────────────────────────────────────────

// getLuaPluginStats returns lightweight in-process stats for all loaded Lua plugins.
// Response: {"plugin_id": {"log_lines": N, "loaded": true}, ...}
func getLuaPluginStats(w http.ResponseWriter, r *http.Request) {
	type pluginStat struct {
		LogLines int  `json:"log_lines"`
		Loaded   bool `json:"loaded"`
	}
	result := map[string]pluginStat{}
	for _, mf := range engine.LuaPlugins.GetMeta() {
		buf := engine.LuaPlugins.GetLogBuffer(mf.ID)
		lines := 0
		if buf != nil {
			lines = len(buf.Lines())
		}
		result[mf.ID] = pluginStat{
			LogLines: lines,
			Loaded:   engine.LuaPlugins.Has(mf.ID),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

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

// findLuaPluginDir scans the plugins directory and returns the path for pluginID.
func findLuaPluginDir(pluginID string) (string, error) {
	entries, err := os.ReadDir(core.AppPath("plugins"))
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := core.AppPath("plugins", e.Name())
		mf, err := readLuaManifest(dir)
		if err == nil && mf.ID == pluginID {
			return dir, nil
		}
	}
	return "", fmt.Errorf("plugin %q not found", pluginID)
}

// safeJoin joins pluginDir with relPath and verifies the result stays inside pluginDir.
func safeJoin(pluginDir, relPath string) (string, bool) {
	abs := filepath.Join(pluginDir, filepath.FromSlash(relPath))
	abs = filepath.Clean(abs)
	base := filepath.Clean(pluginDir) + string(os.PathSeparator)
	if !strings.HasPrefix(abs+string(os.PathSeparator), base) {
		return "", false
	}
	return abs, true
}

// ─── helper ───────────────────────────────────────────────────────────────────

// sanitizeName strips path separators and dots to prevent directory traversal.
func sanitizeName(name string) string {
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, "..", "")
	return name
}

// readLuaManifest is re-exported here so lua_admin.go can use it without
// importing the engine package's unexported function (it's in engine package,
// so we just call the engine version directly).
func readLuaManifest(dir string) (engine.LuaManifest, error) {
	return engine.ReadLuaManifest(dir)
}
