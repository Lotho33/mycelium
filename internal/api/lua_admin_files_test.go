package api

import (
	"os"
	"path/filepath"
	"testing"
)

// The editor only reaches editable files inside the plugin directory, never
// through a symlink.
func TestEditorPath(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.json"), filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"init.lua", "manifest.yaml", "new.lua", "data/x.json"} {
		if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, ok := editorPath(dir, rel); !ok {
			t.Errorf("%q refused", rel)
		}
	}
	for _, rel := range []string{"../x.lua", "link.json", "sub/secret.json", "icon.svg", "run.sh"} {
		if _, ok := editorPath(dir, rel); ok {
			t.Errorf("%q allowed", rel)
		}
	}
}
