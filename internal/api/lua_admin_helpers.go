package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mycelium/internal/core"
	"mycelium/internal/engine"
)

// findLuaPluginDir scans the plugins directory and returns the path for pluginID.
func findLuaPluginDir(pluginID string) (string, error) {
	entries, err := os.ReadDir(core.AppPath("plugins"))
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		// Dot directories are uploads in progress and backups, not plugins
		// (LoadAll skips them too).
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
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

// validPluginNameRe whitelists sanitized plugin/file names. A whitelist, not
// removal of "..": ".." stripped to "" would resolve to plugins/ itself.
// readLuaManifest reads a manifest through the engine.
func readLuaManifest(dir string) (engine.LuaManifest, error) {
	return engine.ReadLuaManifest(dir)
}
