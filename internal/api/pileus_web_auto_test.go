package api

import (
	"os"
	"path/filepath"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

func TestPileusWebNeedsUpdate(t *testing.T) {
	tmp := t.TempDir()
	oldBase := core.BasePath
	core.BasePath = tmp
	t.Cleanup(func() { core.BasePath = oldBase })
	t.Cleanup(func() { _ = managers.Settings.SaveInternal(map[string]any{pileusWebInstalledTagKey: ""}) })
	_ = managers.Settings.SaveInternal(map[string]any{pileusWebInstalledTagKey: ""})

	// Nothing installed: install.
	if !pileusWebNeedsUpdate("o/r", "v1.2.0") {
		t.Fatal("no web app installed: want an update")
	}

	// Installed before releases were tracked: compare with version.json.
	dir := filepath.Join(tmp, "data", "pileus-web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version.json"), []byte(`{"version":"1.2.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if pileusWebNeedsUpdate("o/r", "v1.2.0") {
		t.Fatal("same version installed: want no update")
	}
	if !pileusWebNeedsUpdate("o/r", "v1.3.0") {
		t.Fatal("older version installed: want an update")
	}

	// A recorded release wins: only a different tag (or repository) updates.
	_ = managers.Settings.SaveInternal(map[string]any{pileusWebInstalledTagKey: "o/r@v1.3.0"})
	if pileusWebNeedsUpdate("o/r", "v1.3.0") {
		t.Fatal("recorded release: want no update")
	}
	if !pileusWebNeedsUpdate("o/r", "v1.4.0") || !pileusWebNeedsUpdate("other/r", "v1.3.0") {
		t.Fatal("new release or new repository: want an update")
	}
}
