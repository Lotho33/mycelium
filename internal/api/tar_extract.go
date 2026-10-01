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

// extractTarGzSafe extracts a gzip-compressed tar archive into destDir
// (which must exist), with the same guarantees as extractZipSafe:
//   - path traversal: an entry resolving outside destDir is skipped and
//     logged;
//   - symlinks/hardlinks are refused (fatal);
//   - a single top-level folder shared by every entry is stripped
//     (stripSingleWrapperPrefix);
//   - maxPerFile caps what is written for one entry;
//   - maxTotal caps the uncompressed total; exceeding it aborts
//     (aborted=true) and the caller discards destDir.
//
// A tar stream is sequential, so the archive is read twice: names first
// (to find the wrapper prefix), then contents. logPrefix tags log lines.
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
			// Fixed mode, never the archive's own.
			out, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				continue
			}
			n, _ := io.Copy(out, io.LimitReader(tr, maxPerFile))
			totalExtracted += n
			out.Close()
		default:
			// Other entry types (fifo, device, …): skip.
		}
	}
	return false, totalExtracted, nil
}

// tarGzEntryNames lists every entry name (headers only, no content written).
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
