package api

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

	res, ierr := installPluginZip(tmpPath)
	if ierr != nil {
		http.Error(w, ierr.Error(), ierr.status)
		return
	}
	verb := "installato"
	if res.Update {
		verb = "aggiornato"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"ok":      "true",
		"message": "Plugin Lua " + verb + " e caricato.",
		"id":      res.ID,
		"update":  boolToString(res.Update),
	})
}

// installResult is what installPluginZip installed.
type installResult struct {
	ID      string
	Version string // manifest version
	Update  bool   // it replaced an installed copy
}

// installError carries the HTTP status an install failure maps to.
type installError struct {
	status int
	msg    string
}

func (e *installError) Error() string { return e.msg }

func installErr(status int, msg string) *installError { return &installError{status, msg} }

// installPluginZip installs (or updates) the plugin packed in the ZIP at
// zipPath: the id comes from its manifest.yaml, which may sit at the root or
// inside one top-level folder (a GitHub source archive). An update keeps the
// installed copy until the new one loads, and puts it back on failure. Used by
// the dashboard upload and by installs from a URL.
func installPluginZip(zipPath string) (installResult, *installError) {
	pluginsRoot := filepath.Clean(core.AppPath("plugins"))
	if err := os.MkdirAll(pluginsRoot, 0755); err != nil {
		return installResult{}, installErr(http.StatusInternalServerError, "errore creazione cartella plugins")
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return installResult{}, installErr(http.StatusBadRequest, "ZIP corrotto")
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
		return installResult{}, installErr(http.StatusBadRequest, "ZIP deve contenere manifest.yaml e init.lua")
	}

	// Extract to a staging directory first: the destination is derived from the
	// id declared in manifest.yaml, not from the ZIP's name, so uploading a new
	// version updates the installed plugin.
	stagingDir, err := os.MkdirTemp(pluginsRoot, ".staging-*")
	if err != nil {
		return installResult{}, installErr(http.StatusInternalServerError, "errore creazione staging")
	}
	defer os.RemoveAll(stagingDir) // no-op once renamed into place below

	// maxTotalExtractedSize caps the whole archive (the per-file cap alone
	// doesn't stop many small files). Extraction is shared with the web-app
	// updater (zip_extract.go).
	const maxTotalExtractedSize = 200 << 20 // 200 MiB
	aborted, _, xerr := extractZipSafe(&zr.Reader, stagingDir, 10<<20, maxTotalExtractedSize, "[lua/upload]")
	if xerr != nil {
		return installResult{}, installErr(http.StatusBadRequest, "archivio non valido: "+xerr.Error())
	}
	if aborted {
		return installResult{}, installErr(http.StatusBadRequest, "archivio troppo grande una volta estratto")
	}
	srcDir := pluginRootIn(stagingDir)

	mf, err := readLuaManifest(srcDir)
	if err != nil {
		return installResult{}, installErr(http.StatusBadRequest, "manifest.yaml non valido: "+err.Error())
	}
	if err := engine.CheckMinCore(mf); err != nil {
		return installResult{}, installErr(http.StatusBadRequest, err.Error())
	}
	// readLuaManifest already validated mf.ID against the id whitelist: use it
	// as the directory name.
	pluginName := mf.ID
	destDir := core.AppPath("plugins", pluginName)
	destDirClean := filepath.Clean(destDir)
	if destDirClean == pluginsRoot || filepath.Dir(destDirClean) != pluginsRoot {
		return installResult{}, installErr(http.StatusBadRequest, "id plugin non valido")
	}

	// "shared" is not a plugin: its modules are preloaded into every plugin, so
	// an upload with that id would run code under other plugins' identity.
	if pluginName == "shared" {
		return installResult{}, installErr(http.StatusBadRequest, "id plugin riservato: shared")
	}
	// One install at a time: two uploads of the same id would race on the
	// directory.
	pluginUploadMu.Lock()
	defer pluginUploadMu.Unlock()

	isUpdate := engine.LuaPlugins.Has(pluginName)
	if isUpdate {
		// Update the directory the plugin actually loads from, which may not be
		// plugins/<id> (a second copy would clash on the next boot).
		if dir, err := findLuaPluginDir(pluginName); err == nil {
			destDir = dir
		}
	}
	// An existing directory that isn't this plugin is not ours to merge into.
	if !isUpdate {
		if _, err := os.Stat(destDir); err == nil {
			if existing, merr := readLuaManifest(destDir); merr != nil || existing.ID != pluginName {
				return installResult{}, installErr(http.StatusConflict, "la cartella plugins/"+pluginName+" esiste già e non contiene questo plugin")
			}
			isUpdate = true // installed but not loaded
		}
	}

	// The current version is kept until the new one loads: on failure it is
	// put back and reloaded.
	var backupDir string
	if isUpdate {
		backupDir = filepath.Join(pluginsRoot, fmt.Sprintf(".backup-%s-%d", pluginName, time.Now().UnixNano()))
		if err := os.CopyFS(backupDir, os.DirFS(destDir)); err != nil {
			os.RemoveAll(backupDir)
			return installResult{}, installErr(http.StatusInternalServerError, "errore backup della versione installata")
		}
		defer os.RemoveAll(backupDir) // no-op once restored
		// Hot-swap: unload the old pool before its directory changes. Files are
		// merged, never wiped: runtime data (*_cache.json, *_index.json) survives
		// unless the ZIP itself contains such files.
		engine.LuaPlugins.UnloadPlugin(pluginName)
	}
	restore := func() {
		if backupDir == "" {
			os.RemoveAll(destDir)
			return
		}
		if err := os.RemoveAll(destDir); err == nil && os.Rename(backupDir, destDir) == nil {
			if lerr := engine.LuaPlugins.LoadPlugin(destDir); lerr != nil {
				log.Printf("[lua/upload] %s: ripristino della versione precedente non caricato: %v", pluginName, lerr)
			} else {
				log.Printf("[lua/upload] %s: aggiornamento fallito, ripristinata la versione precedente", pluginName)
			}
		}
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return installResult{}, installErr(http.StatusInternalServerError, "errore creazione directory")
	}
	if isUpdate {
		// Code comes only from the new ZIP: drop the old .lua modules first so a
		// removed one doesn't stay require-able.
		if err := removeLuaFiles(destDir); err != nil {
			restore()
			return installResult{}, installErr(http.StatusInternalServerError, "errore rimozione moduli precedenti: "+err.Error())
		}
	}
	if err := mergeDirInto(srcDir, destDir); err != nil {
		restore()
		return installResult{}, installErr(http.StatusInternalServerError, "errore installazione file: "+err.Error())
	}

	if err := engine.LuaPlugins.LoadPlugin(destDir); err != nil {
		restore()
		msg := "plugin estratto ma caricamento fallito: " + err.Error()
		if isUpdate {
			msg += " (ripristinata la versione precedente)"
		}
		return installResult{}, installErr(http.StatusInternalServerError, msg)
	}

	verb := "installato"
	if isUpdate {
		verb = "aggiornato"
	}
	log.Printf("[lua/upload] plugin %s: %s", verb, pluginName)
	return installResult{ID: pluginName, Version: mf.Version, Update: isUpdate}, nil
}

// pluginRootIn is the directory of an extracted archive that holds
// manifest.yaml: the archive root, or its single top-level folder (GitHub
// source archives and ZIPs made by compressing a folder).
func pluginRootIn(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "manifest.yaml")); err == nil {
		return dir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dir
	}
	var only string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "__MACOSX") {
			continue
		}
		if !e.IsDir() || only != "" {
			return dir
		}
		only = e.Name()
	}
	if only == "" {
		return dir
	}
	sub := filepath.Join(dir, only)
	if _, err := os.Stat(filepath.Join(sub, "manifest.yaml")); err == nil {
		return sub
	}
	return dir
}

var pluginUploadMu sync.Mutex

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
		forgetPluginSource(body.PluginID)
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
