package api

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// extractTarGzSafe extracts a gzip-compressed tar archive (tarGzPath) into
// destDir (which must already exist) — the tar.gz mirror of extractZipSafe,
// written for the same reason: a release asset can legitimately arrive as
// either format (Pileus's web build shipped as .zip historically, switched to
// .tar.gz as of v1.2.7, and may switch back or change again), and this
// endpoint shouldn't assume one is permanent. Same guarantees as
// extractZipSafe:
//
//   - tar-slip / path traversal: every resolved entry path must stay under
//     destDir — an entry that would escape it (e.g. "../../etc/passwd") is
//     skipped and logged, not fatal to the rest of the archive. Identical
//     check to extractZipSafe's: never trust the path inside the archive.
//   - symlinks/hardlinks are refused outright (fatal, not skipped): a tar
//     entry can create a link pointing anywhere on the filesystem, a vector
//     zip doesn't have in the same shape. Extraction stops immediately with
//     a clear error instead of ever creating one.
//   - a single top-level wrapper folder shared by every entry is detected and
//     stripped (stripSingleWrapperPrefix, shared with the zip side), so both
//     "index.html" and "web/index.html" archives extract to the same layout.
//   - maxPerFile caps how much of one entry's content is written (a header
//     lying about Size for a small entry doesn't matter — the copy itself is
//     bounded).
//   - maxTotal caps the archive's uncompressed sum. Exceeding it aborts the
//     whole extraction (aborted=true) — unlike a single traversal attempt,
//     this is treated as fatal: the caller is expected to discard destDir's
//     partial contents.
//
// Unlike zip (whose central directory lists every entry name up front),
// a tar stream is sequential — so the wrapper-folder prefix can't be known
// until every entry has been seen. extractTarGzSafe makes two passes over
// tarGzPath (opening and decompressing it twice) rather than buffering the
// whole archive in memory: a cheap trade for a build asset of this size, and
// it keeps the same "decode, then extract" shape extractZipSafe gets for
// free from archive/zip.
//
// logPrefix tags log lines (e.g. "[pileus-web]") the same way extractZipSafe's
// does.
func extractTarGzSafe(tarGzPath, destDir string, maxPerFile, maxTotal int64, logPrefix string) (aborted bool, totalExtracted int64, err error) {
	names, err := tarGzEntryNames(tarGzPath)
	if err != nil {
		return false, 0, err
	}
	stripPrefix := stripSingleWrapperPrefix(names)

	f, err := os.Open(tarGzPath)
	if err != nil {
		return false, 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, 0, fmt.Errorf("gzip corrotto: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	extractAbs := filepath.Clean(destDir) + string(os.PathSeparator)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, totalExtracted, fmt.Errorf("tar corrotto: %w", err)
		}

		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			return false, totalExtracted, fmt.Errorf("%s entry di tipo link rifiutata (%s): un archivio web non dovrebbe contenerne", logPrefix, hdr.Name)
		}

		rel := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "./")
		rel = strings.TrimPrefix(rel, stripPrefix)
		if rel == "" || rel == "/" {
			continue
		}
		fpath := filepath.Join(destDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(filepath.Clean(fpath)+string(os.PathSeparator), extractAbs) {
			log.Printf("%s path traversal bloccato: %s", logPrefix, hdr.Name)
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(fpath, 0755)
		case tar.TypeReg:
			if totalExtracted+hdr.Size > maxTotal {
				log.Printf("%s archivio oltre il limite totale (%d MiB), estrazione interrotta", logPrefix, maxTotal>>20)
				return true, totalExtracted, nil
			}
			os.MkdirAll(filepath.Dir(fpath), 0755)
			out, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode&0o777))
			if err != nil {
				continue
			}
			n, _ := io.Copy(out, io.LimitReader(tr, maxPerFile))
			totalExtracted += n
			out.Close()
		default:
			// Anything else (fifo, device, ...) isn't expected in a web
			// build and isn't a traversal/link risk on its own — skip it.
		}
	}
	return false, totalExtracted, nil
}

// tarGzEntryNames decompresses tarGzPath just far enough to collect every
// entry's name — no file content is written anywhere — so
// stripSingleWrapperPrefix can decide on a wrapper prefix before the real
// extraction pass runs. Reading each entry's header without consuming its
// body is exactly what archive/tar's Next() does (it auto-skips whatever of
// the previous entry's body wasn't read), so this is a cheap header-only walk
// rather than a second full extraction.
func tarGzEntryNames(tarGzPath string) ([]string, error) {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip corrotto: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar corrotto: %w", err)
		}
		names = append(names, hdr.Name)
	}
	return names, nil
}
