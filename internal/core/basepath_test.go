package core

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests must never run on the developer's real data/: the devcontainer sets
// MYCELIUM_BASE to the workspace, and a test that resets settings would wipe
// it (it did, once).
func TestBasePath_IgnoresMyceliumBaseUnderTest(t *testing.T) {
	env := os.Getenv("MYCELIUM_BASE")
	if env == "" {
		t.Skip("MYCELIUM_BASE non impostato: niente da verificare")
	}
	abs, _ := filepath.Abs(env)
	if BasePath == abs {
		t.Fatalf("BasePath = MYCELIUM_BASE (%s) durante i test: i test scriverebbero nei dati reali", abs)
	}
}
