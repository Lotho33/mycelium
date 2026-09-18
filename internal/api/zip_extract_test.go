package api

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// buildZip builds an in-memory zip.Reader from a name→content map. Map
// iteration order is randomized but doesn't matter for the tests below —
// zipStripSinglePrefix's behavior here only depends on whether any entry
// lacks a "/" in its name, not on ordering.
func buildZip(t *testing.T, files map[string]string) *zip.Reader {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	return r
}

// TestExtractZipSafe_RejectsPathTraversal is the regression test for the
// zip-slip protection shared between the Lua plugin uploader
// (uploadLuaPlugin) and the Pileus web-app updater (installPileusWebAsset):
// an entry named "../../etc/passwd" must never land outside destDir, while
// the rest of an otherwise legitimate archive still extracts normally.
func TestExtractZipSafe_RejectsPathTraversal(t *testing.T) {
	destDir := t.TempDir()
	zr := buildZip(t, map[string]string{
		"../../etc/passwd": "pwned",
		"index.html":       "<html></html>",
	})

	aborted, _ := extractZipSafe(zr, destDir, 10<<20, 200<<20, "[test]")
	if aborted {
		t.Fatalf("extraction unexpectedly aborted")
	}

	// Nothing named "passwd" should exist anywhere under destDir — the
	// traversal entry must be skipped outright, not merely relocated inside.
	found := false
	filepath.Walk(destDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Base(path) == "passwd" {
			found = true
		}
		return nil
	})
	if found {
		t.Fatalf("traversal entry was extracted somewhere under destDir instead of being skipped")
	}

	// Nor should it have escaped upward from destDir.
	escaped := filepath.Join(filepath.Dir(filepath.Dir(destDir)), "etc", "passwd")
	if _, err := os.Stat(escaped); err == nil {
		t.Fatalf("traversal entry escaped destDir to %s", escaped)
	}

	// The legitimate entry must still extract correctly.
	data, err := os.ReadFile(filepath.Join(destDir, "index.html"))
	if err != nil || string(data) != "<html></html>" {
		t.Fatalf("index.html not extracted correctly: data=%q err=%v", data, err)
	}
}

// TestExtractZipSafe_AbortsOverTotalCap covers the total-size guard: a
// zip whose declared uncompressed size exceeds maxTotal must abort the whole
// extraction rather than silently truncate it.
func TestExtractZipSafe_AbortsOverTotalCap(t *testing.T) {
	destDir := t.TempDir()
	big := make([]byte, 1024)
	zr := buildZip(t, map[string]string{"big.bin": string(big)})

	aborted, _ := extractZipSafe(zr, destDir, 10<<20, 100 /* 100 bytes cap */, "[test]")
	if !aborted {
		t.Fatalf("expected extraction to abort when the archive exceeds maxTotal")
	}
}

// TestExtractZipSafe_StripsSingleWrapperFolder covers the case where every
// entry shares one top-level folder (as when an archive was made by zipping
// the build directory itself) — it should be stripped so files land directly
// in destDir.
func TestExtractZipSafe_StripsSingleWrapperFolder(t *testing.T) {
	destDir := t.TempDir()
	zr := buildZip(t, map[string]string{
		"build/index.html":   "<html></html>",
		"build/main.dart.js": "console.log(1)",
	})

	aborted, _ := extractZipSafe(zr, destDir, 10<<20, 200<<20, "[test]")
	if aborted {
		t.Fatalf("extraction unexpectedly aborted")
	}
	if _, err := os.Stat(filepath.Join(destDir, "index.html")); err != nil {
		t.Fatalf("index.html should have been extracted at destDir root after stripping the wrapper folder: %v", err)
	}
}
