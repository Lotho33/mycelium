package core

import (
	"log"
	"os"
	"path/filepath"
)

// BasePath è la directory che contiene il binario del server.
// Tutti i percorsi relativi (web/, plugins/, data/) vanno costruiti da qui.
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
	// Segue eventuali symlink (es. `go run` crea un tmp symlink)
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	BasePath = filepath.Dir(resolved)
}

// AppPath costruisce un percorso assoluto relativo alla directory del binario.
func AppPath(parts ...string) string {
	all := append([]string{BasePath}, parts...)
	return filepath.Join(all...)
}
