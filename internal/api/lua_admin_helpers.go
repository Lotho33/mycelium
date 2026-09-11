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
