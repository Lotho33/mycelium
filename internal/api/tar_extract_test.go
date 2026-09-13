package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// tarGzEntry is one entry to write with buildTarGz — either a regular file
// (Body != "" or explicitly a plain file) or a link (LinkTo != "").
type tarGzEntry struct {
	Name    string
	Body    string
	LinkTo  string // if non-empty, written as a tar.TypeSymlink pointing here
	Symlink bool
}

// buildTarGz writes a gzip-compressed tar archive to disk (a real file, since
// extractTarGzSafe reopens its input for a second pass) from entries and
// returns its path.
func buildTarGz(t *testing.T, entries []tarGzEntry) string {
	t.Helper()
	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if e.Symlink {
			if err := tw.WriteHeader(&tar.Header{
				Name:     e.Name,
				Typeflag: tar.TypeSymlink,
				Linkname: e.LinkTo,
				Mode:     0777,
			}); err != nil {
				t.Fatalf("tar header (symlink) %q: %v", e.Name, err)
			}
			continue
		}
		body := []byte(e.Body)
		if err := tw.WriteHeader(&tar.Header{
			Name: e.Name,
			Mode: 0644,
			Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("tar header %q: %v", e.Name, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("tar write %q: %v", e.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write tar.gz: %v", err)
	}
	return path
}

// TestExtractTarGzSafe_RejectsPathTraversal is the tar.gz mirror of
// TestExtractZipSafe_RejectsPathTraversal: a "../../etc/passwd" entry must
// never land outside destDir, while the rest of the archive extracts fine.
func TestExtractTarGzSafe_RejectsPathTraversal(t *testing.T) {
	destDir := t.TempDir()
	archive := buildTarGz(t, []tarGzEntry{
		{Name: "../../etc/passwd", Body: "pwned"},
		{Name: "index.html", Body: "<html></html>"},
	})

	aborted, _, err := extractTarGzSafe(archive, destDir, 10<<20, 200<<20, "[test]")
	if err != nil {
		t.Fatalf("extractTarGzSafe: %v", err)
	}
	if aborted {
		t.Fatalf("extraction unexpectedly aborted")
	}

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

	escaped := filepath.Join(filepath.Dir(filepath.Dir(destDir)), "etc", "passwd")
	if _, err := os.Stat(escaped); err == nil {
		t.Fatalf("traversal entry escaped destDir to %s", escaped)
	}

	data, err := os.ReadFile(filepath.Join(destDir, "index.html"))
	if err != nil || string(data) != "<html></html>" {
		t.Fatalf("index.html not extracted correctly: data=%q err=%v", data, err)
	}
}

// TestExtractTarGzSafe_RejectsSymlink covers the tar-specific hardening zip
// doesn't need in the same shape: a symlink entry must fail the whole
// extraction with a clear error instead of ever being created.
func TestExtractTarGzSafe_RejectsSymlink(t *testing.T) {
	destDir := t.TempDir()
	archive := buildTarGz(t, []tarGzEntry{
		{Name: "index.html", Body: "<html></html>"},
		{Name: "evil-link", Symlink: true, LinkTo: "/etc/passwd"},
	})

	_, _, err := extractTarGzSafe(archive, destDir, 10<<20, 200<<20, "[test]")
	if err == nil {
		t.Fatalf("expected an error for a symlink entry, got nil")
	}

	if _, statErr := os.Lstat(filepath.Join(destDir, "evil-link")); statErr == nil {
		t.Fatalf("symlink entry was created under destDir instead of being rejected")
	}
}

// TestExtractTarGzSafe_RejectsHardlink covers tar.TypeLink the same way
// TestExtractTarGzSafe_RejectsSymlink covers tar.TypeSymlink.
func TestExtractTarGzSafe_RejectsHardlink(t *testing.T) {
	destDir := t.TempDir()
	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Mode: 0644, Size: 5}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	tw.Write([]byte("hello"))
	if err := tw.WriteHeader(&tar.Header{
		Name:     "hard-link",
		Typeflag: tar.TypeLink,
		Linkname: "index.html",
	}); err != nil {
		t.Fatalf("tar header (hardlink): %v", err)
	}
	tw.Close()
	gz.Close()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write tar.gz: %v", err)
	}

	_, _, err := extractTarGzSafe(path, destDir, 10<<20, 200<<20, "[test]")
	if err == nil {
		t.Fatalf("expected an error for a hardlink entry, got nil")
	}
}

// TestExtractTarGzSafe_AbortsOverTotalCap is the tar.gz mirror of
// TestExtractZipSafe_AbortsOverTotalCap.
func TestExtractTarGzSafe_AbortsOverTotalCap(t *testing.T) {
	destDir := t.TempDir()
	big := make([]byte, 1024)
	archive := buildTarGz(t, []tarGzEntry{{Name: "big.bin", Body: string(big)}})

	aborted, _, err := extractTarGzSafe(archive, destDir, 10<<20, 100 /* 100 bytes cap */, "[test]")
	if err != nil {
		t.Fatalf("extractTarGzSafe: %v", err)
	}
	if !aborted {
		t.Fatalf("expected extraction to abort when the archive exceeds maxTotal")
	}
}

// TestExtractTarGzSafe_StripsSingleWrapperFolder is the tar.gz mirror of
// TestExtractZipSafe_StripsSingleWrapperFolder.
func TestExtractTarGzSafe_StripsSingleWrapperFolder(t *testing.T) {
	destDir := t.TempDir()
	archive := buildTarGz(t, []tarGzEntry{
		{Name: "build/index.html", Body: "<html></html>"},
		{Name: "build/main.dart.js", Body: "console.log(1)"},
	})

	aborted, _, err := extractTarGzSafe(archive, destDir, 10<<20, 200<<20, "[test]")
	if err != nil {
		t.Fatalf("extractTarGzSafe: %v", err)
	}
	if aborted {
		t.Fatalf("extraction unexpectedly aborted")
	}
	if _, err := os.Stat(filepath.Join(destDir, "index.html")); err != nil {
		t.Fatalf("index.html should have been extracted at destDir root after stripping the wrapper folder: %v", err)
	}
}

// TestExtractTarGzSafe_NoWrapperFolder covers the case the real
// Lotho33/pileus v1.2.7 web asset actually uses: every entry already sits at
// the archive root (as "./index.html", "./manifest.json", ...) — no wrapper
// folder to strip, and the "./" prefix itself must not confuse either the
// stripping logic or path-traversal containment.
func TestExtractTarGzSafe_NoWrapperFolder(t *testing.T) {
	destDir := t.TempDir()
	archive := buildTarGz(t, []tarGzEntry{
		{Name: "./index.html", Body: "<html></html>"},
		{Name: "./main.dart.js", Body: "console.log(1)"},
	})

	aborted, _, err := extractTarGzSafe(archive, destDir, 10<<20, 200<<20, "[test]")
	if err != nil {
		t.Fatalf("extractTarGzSafe: %v", err)
	}
	if aborted {
		t.Fatalf("extraction unexpectedly aborted")
	}
	data, err := os.ReadFile(filepath.Join(destDir, "index.html"))
	if err != nil || string(data) != "<html></html>" {
		t.Fatalf("index.html not extracted correctly: data=%q err=%v", data, err)
	}
}
