package core

import (
	"log"
	"os"
	"path/filepath"
)

// BasePath is the directory holding the server binary; relative paths
// (web/, plugins/, data/) are built from it.
var BasePath string

func init() {
	if override := os.Getenv("MYCELIUM_BASE"); override != "" {
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
