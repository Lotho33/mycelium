package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// validPluginNameRe whitelists sanitized plugin/file names. A whitelist, not
// removal of "..": ".." stripped to "" would resolve to plugins/ itself.
var validPluginNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// sanitizeName strips path separators and validates the remainder against a
// strict whitelist to prevent directory traversal. It returns an error
// instead of a best-effort string so callers can never mistake a rejected
// name for a usable one — an empty, ".", or ".." result must never reach
// code that builds a filesystem path from it.
func sanitizeName(name string) (string, error) {
	name = filepath.Base(filepath.Clean(name))
	if name == "" || name == "." || name == ".." || name == string(os.PathSeparator) {
		return "", fmt.Errorf("nome non valido: %q", name)
	}
	if !validPluginNameRe.MatchString(name) {
		return "", fmt.Errorf("nome contiene caratteri non consentiti: %q", name)
	}
	return name, nil
}

// readLuaManifest reads a manifest through the engine.
func readLuaManifest(dir string) (engine.LuaManifest, error) {
	return engine.ReadLuaManifest(dir)
}
