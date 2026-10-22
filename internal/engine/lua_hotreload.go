package engine

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"mycelium/internal/core"
)

// hotReloadDebounce merges the burst of events one save produces (editors
// write, rename, chmod) into one reload.
const hotReloadDebounce = 400 * time.Millisecond

// watchHotReload watches the plugin directory (via fsnotify, or mtime
// polling when it is unavailable): a changed .lua reloads the pool, a
// changed manifest.yaml reloads the whole plugin.
func (m *LuaPluginManager) watchHotReload(p *LuaPlugin) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[lua] fsnotify unavailable for %s, using mtime polling: %v", p.Manifest.ID, err)
		m.watchHotReloadPolling(p)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(p.Dir); err != nil {
		log.Printf("[lua] fsnotify watch %s: %v — using mtime polling", p.Dir, err)
		m.watchHotReloadPolling(p)
		return
	}

	log.Printf("[lua] hot-reload watching %s via fsnotify", p.Manifest.ID)
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	pending, manifest := false, false
	for {
		select {
		case <-p.stopHR:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) && !event.Has(fsnotify.Rename) {
				continue
			}
			base := filepath.Base(event.Name)
			switch {
			case base == "manifest.yaml":
				manifest = true
			case filepath.Ext(base) == ".lua" && !strings.HasPrefix(base, "."):
			default:
				continue
			}
			pending = true
			debounce.Reset(hotReloadDebounce)
		case <-debounce.C:
			if !pending {
				continue
			}
			if m.hotReload(p, manifest) {
				return // the plugin was reloaded whole: its new watcher takes over
			}
			pending, manifest = false, false
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[lua] fsnotify error for %s: %v", p.Manifest.ID, err)
		}
	}
}

// watchHotReloadPolling is watchHotReload by mtime, every 2 seconds.
func (m *LuaPluginManager) watchHotReloadPolling(p *LuaPlugin) {
	lastLua, lastManifest := luaFilesStamp(p.Dir), fileMod(filepath.Join(p.Dir, "manifest.yaml"))
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopHR:
			return
		case <-ticker.C:
			lua, mf := luaFilesStamp(p.Dir), fileMod(filepath.Join(p.Dir, "manifest.yaml"))
			if lua.Equal(lastLua) && mf.Equal(lastManifest) {
				continue
			}
			manifest := !mf.Equal(lastManifest)
			lastLua, lastManifest = lua, mf
			if m.hotReload(p, manifest) {
				return
			}
		}
	}
}

// hotReload applies a change of p's files: the whole plugin when the
// manifest changed (entrypoints, tasks, egress may have), else its pool.
// It reports whether the plugin was reloaded whole. A bad reload (a syntax
// error, a panic deep in the VM setup) must not end hot-reload.
func (m *LuaPluginManager) hotReload(p *LuaPlugin, manifest bool) (whole bool) {
	defer core.Guard("lua/hot-reload:" + p.Manifest.ID)
	if manifest {
		if err := m.LoadPlugin(p.Dir); err != nil {
			log.Printf("[lua] hot-reload %s (manifest): %v", p.Manifest.ID, err)
			return false
		}
		log.Printf("[lua] hot-reload %s: OK (manifest, plugin ricaricato)", p.Manifest.ID)
		return true
	}
	if err := p.Pool.Reload(); err != nil {
		log.Printf("[lua] hot-reload %s: %v", p.Manifest.ID, err)
		return false
	}
	pluginIndexes.dropPlugin(p.Manifest.ID)
	log.Printf("[lua] hot-reload %s: OK", p.Manifest.ID)
	return false
}

// luaFilesStamp is the latest mtime among dir's .lua files.
func luaFilesStamp(dir string) time.Time {
	var latest time.Time
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".lua" {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}
