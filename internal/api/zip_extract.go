package api

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// stripSingleWrapperPrefix detects a single top-level folder wrapping every
// entry of names (an archive of a directory: "myplugin/manifest.yaml") and
// returns that prefix ("<dir>/"), or "" when entries don't all share one.
// Shared by the zip and tar.gz extractors.
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

// zipStripSinglePrefix is stripSingleWrapperPrefix for a zip's entries.
func zipStripSinglePrefix(files []*zip.File) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return stripSingleWrapperPrefix(names)
}

// extractZipSafe extracts every file of zr into destDir (which must exist),
// shared by the plugin uploader and the web-app updater:
//   - path traversal: an entry resolving outside destDir is skipped and
//     logged;
//   - a single top-level folder shared by every entry is stripped;
//   - an entry over maxPerFile (whatever its header claims), or one that
//     can't be written or read whole (a bad checksum included), is an error:
//     the caller discards destDir rather than install a damaged build;
//   - maxTotal caps the uncompressed total; exceeding it aborts
//     (aborted=true) and the caller discards destDir.
//
// logPrefix tags log lines.
func extractZipSafe(zr *zip.Reader, destDir string, maxPerFile, maxTotal int64, logPrefix string) (aborted bool, totalExtracted int64, err error) {
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
		if !f.Mode().IsRegular() {
			// Symlinks, devices, ...: never part of a plugin or web build.
			log.Printf("%s entry non regolare ignorata: %s", logPrefix, f.Name)
			continue
		}
		if totalExtracted+int64(f.UncompressedSize64) > maxTotal {
			log.Printf("%s archivio oltre il limite totale (%d MiB), estrazione interrotta", logPrefix, maxTotal>>20)
			return true, totalExtracted, nil
		}
		rc, err := f.Open()
		if err != nil {
			return false, totalExtracted, fmt.Errorf("%s: %w", f.Name, err)
		}
		n, err := writeExtracted(fpath, rc, maxPerFile)
		rc.Close()
		totalExtracted += n
		if err != nil {
			return false, totalExtracted, fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	return false, totalExtracted, nil
}

// writeExtracted writes r to fpath (fixed mode, never the archive's own:
// setuid bits, world-writable), refusing more than maxPerFile bytes.
func writeExtracted(fpath string, r io.Reader, maxPerFile int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(r, maxPerFile+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxPerFile {
		err = fmt.Errorf("file oltre il limite di %d MiB", maxPerFile>>20)
	}
	return n, err
}
