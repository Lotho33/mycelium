package engine

import (
	"log"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"mycelium/internal/core"
)

// watchHotReload watches the plugin directory for .lua file changes using
// inotify (via fsnotify) and reloads the pool on write/create events.
// Falls back to mtime polling if fsnotify is unavailable.
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
	for {
		select {
		case <-p.stopHR:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if (event.Has(fsnotify.Write) || event.Has(fsnotify.Create)) &&
				filepath.Ext(event.Name) == ".lua" {
				// One bad reload (a plugin author's syntax error triggering an
				// unexpected panic deep in the Lua VM setup) must not kill
				// hot-reload for this plugin forever — the outer SafeGo only
				// catches one panic for the goroutine's whole lifetime.
				func() {
					defer core.Guard("lua/hot-reload-event:" + p.Manifest.ID)
					if err := p.Pool.Reload(); err != nil {
						log.Printf("[lua] hot-reload %s: %v", p.Manifest.ID, err)
					} else {
						log.Printf("[lua] hot-reload %s: OK (%s)", p.Manifest.ID, filepath.Base(event.Name))
					}
				}()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[lua] fsnotify error for %s: %v", p.Manifest.ID, err)
		}
	}
}
