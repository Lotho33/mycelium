package api

import (
	"archive/zip"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// stripSingleWrapperPrefix detects a single top-level folder wrapping every
// entry in names (as when an archive was made by compressing a directory
// itself, so every path reads "myplugin/manifest.yaml" instead of
// "manifest.yaml") and returns that prefix ("<dir>/") to strip from every
// entry name. Returns "" when entries don't all share one common top-level
// folder (including the case where at least one entry already sits at the
// archive root). Format-agnostic — driven purely off entry name strings, so
// both zipStripSinglePrefix (archive/zip) and extractTarGzSafe
// (tar_extract.go) share this one implementation instead of each keeping its
// own copy of the same logic.
func stripSingleWrapperPrefix(names []string) string {
	stripPrefix := ""
	for _, raw := range names {
		name := strings.TrimPrefix(filepath.ToSlash(raw), "./")
		if name == "" || name == "/" {
			continue
		}
		j := strings.IndexByte(name, '/')
		if j < 0 {
			// a file sits at the archive root → no common wrapper folder
			return ""
		}
		seg := name[:j+1] // "<dir>/"
		if stripPrefix == "" {
			stripPrefix = seg
		} else if stripPrefix != seg {
			return ""
		}
	}
	return stripPrefix
}

// zipStripSinglePrefix is stripSingleWrapperPrefix specialized to a zip
// file's entry list. See stripSingleWrapperPrefix for the actual logic.
func zipStripSinglePrefix(files []*zip.File) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return stripSingleWrapperPrefix(names)
}

// extractZipSafe extracts every file in zr into destDir (which must already
// exist), the shared hardening originally written for the Lua plugin
// uploader (uploadLuaPlugin) — reused here so a second zip-accepting endpoint
// (the Pileus web-app updater) doesn't have to reimplement, or subtly
// diverge from, the same protections:
//
//   - zip-slip / path traversal: every resolved entry path must stay under
//     destDir — an entry that would escape it (e.g. "../../etc/passwd") is
//     skipped and logged, not fatal to the rest of the archive.
//   - a single top-level wrapper folder shared by every entry is detected and
//     stripped (zipStripSinglePrefix), so both "manifest.yaml" and
//     "myplugin/manifest.yaml" archives extract to the same layout.
//   - maxPerFile caps how much of one entry's content is written (a large
//     UncompressedSize64 lies for a small entry doesn't matter — the copy
//     itself is bounded).
//   - maxTotal caps the archive's uncompressed sum. Exceeding it aborts the
//     whole extraction (aborted=true) — unlike a single traversal attempt,
//     this is treated as fatal: the caller is expected to discard destDir's
//     partial contents.
//
// logPrefix tags log lines (e.g. "[lua/upload]", "[pileus-web]") so the two
// callers stay distinguishable in the log.
func extractZipSafe(zr *zip.Reader, destDir string, maxPerFile, maxTotal int64, logPrefix string) (aborted bool, totalExtracted int64) {
	extractAbs := filepath.Clean(destDir) + string(os.PathSeparator)
	stripPrefix := zipStripSinglePrefix(zr.File)

	for _, f := range zr.File {
		rel := filepath.ToSlash(f.Name)
		rel = strings.TrimPrefix(rel, stripPrefix)
		if rel == "" {
			continue
		}
		fpath := filepath.Join(destDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(filepath.Clean(fpath)+string(os.PathSeparator), extractAbs) {
			log.Printf("%s path traversal bloccato: %s", logPrefix, f.Name)
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, 0755)
			continue
		}
		if totalExtracted+int64(f.UncompressedSize64) > maxTotal {
			log.Printf("%s archivio oltre il limite totale (%d MiB), estrazione interrotta", logPrefix, maxTotal>>20)
			return true, totalExtracted
		}
		os.MkdirAll(filepath.Dir(fpath), 0755)
		out, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			out.Close()
			continue
		}
		n, _ := io.Copy(out, io.LimitReader(rc, maxPerFile))
		totalExtracted += n
		out.Close()
		rc.Close()
	}
	return false, totalExtracted
}
