package core

import (
	"log"
	"os"
	"path/filepath"
	"testing"
)

// BasePath is the directory holding the server binary; relative paths
// (web/, plugins/, data/) are built from it.
var BasePath string

func init() {
	// Under `go test` MYCELIUM_BASE is ignored: the devcontainer sets it to
	// the workspace, and tests that write settings, reset the server or wipe
	// data would otherwise do it to the developer's real data/. Tests get the
	// test binary's own temporary directory, as they do in CI.
	if override := os.Getenv("MYCELIUM_BASE"); override != "" && !testing.Testing() {
		// Absolute, because ffmpeg runs with the job's work dir as cwd.
		if abs, err := filepath.Abs(override); err == nil {
			override = abs
		}
		BasePath = override
		return
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("⚠️ Impossibile determinare il percorso del binario: %v — uso directory corrente", err)
		BasePath, _ = os.Getwd()
		return
	}
	// Follow symlinks (e.g. `go run`'s temporary binary).
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	BasePath = filepath.Dir(resolved)
}

// AppPath builds an absolute path relative to BasePath.
func AppPath(parts ...string) string {
	all := append([]string{BasePath}, parts...)
	return filepath.Join(all...)
}
