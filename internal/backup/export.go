package backup

import (
	"archive/zip"
	"compress/flate"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// serverLabel names this server in backups: the address clients reach it at
// when configured (in Docker the hostname is just the container id), else
// the hostname.
func serverLabel() string {
	if h := strings.TrimSpace(managers.Settings.GetString("server_host", "")); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}

// FileName is the suggested name of a backup made now.
func FileName(now time.Time) string {
	host := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return -1
	}, strings.ReplaceAll(serverLabel(), ".", "-"))
	if host == "" {
		host = "server"
	}
	return fmt.Sprintf("mycelium-%s-%s%s", host, now.Format("20060102-1504"), Ext)
}

// Export writes a full backup to dst, sealed with passphrase when it isn't
// empty. The archive is built in a temporary file first: a failure halfway
// never reaches dst as a truncated download.
func Export(dst io.Writer, passphrase string) error {
	if err := os.MkdirAll(backupsDir(), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(backupsDir(), ".export-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if err := writeArchive(tmp); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if passphrase != "" {
		return encryptStream(dst, tmp, passphrase)
	}
	_, err = io.Copy(dst, tmp)
	return err
}

// ExportToFile writes a backup to path (atomically).
func ExportToFile(path, passphrase string) error {
	part := path + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := Export(f, passphrase); err != nil {
		f.Close()
		os.Remove(part)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, path)
}

func writeArchive(w io.Writer) error {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestCompression)
	})

	man, _ := json.MarshalIndent(Manifest{
		Format:        formatName,
		FormatVersion: formatVersion,
		CoreVersion:   core.Version,
		CreatedAt:     time.Now().Unix(),
		Hostname:      serverLabel(),
	}, "", "  ")
	if err := addBytes(zw, entryManifest, man); err != nil {
		return err
	}

	// config.json as stored, minus the keys that describe this machine.
	if raw, err := os.ReadFile(core.AppPath("data", "config.json")); err == nil {
		cfg := map[string]any{}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("config.json non leggibile: %w", err)
		}
		for k := range cfg {
			if sec, _ := classifyKey(k); sec == "" {
				delete(cfg, k)
			}
		}
		b, _ := json.MarshalIndent(cfg, "", "    ")
		if err := addBytes(zw, entryConfig, b); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if managers.DB != nil {
		if err := addDBSnapshot(zw); err != nil {
			return fmt.Errorf("database: %w", err)
		}
	}

	if err := addFlatDir(zw, core.AppPath("data", "avatars"), dirAvatars); err != nil {
		return fmt.Errorf("foto profili: %w", err)
	}
	if err := addFlatDir(zw, core.AppPath("data", "wireproxy"), dirWireproxy); err != nil {
		return fmt.Errorf("configurazioni WireGuard: %w", err)
	}
	if err := addPlugins(zw); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	return zw.Close()
}

func addDBSnapshot(zw *zip.Writer) error {
	dir, err := os.MkdirTemp(backupsDir(), ".dbsnap-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	snap := filepath.Join(dir, "mycelium.db")
	if err := managers.DB.SnapshotTo(snap); err != nil {
		return err
	}
	return addFile(zw, entryDB, snap)
}

// addFlatDir adds the regular, non-hidden files directly inside dir under
// prefix. A missing dir adds nothing.
func addFlatDir(zw *zip.Writer, dir, prefix string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !validName(e.Name()) {
			continue
		}
		if err := addFile(zw, prefix+e.Name(), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// addPlugins adds every plugin directory (and the shared modules) without
// cache files, hidden entries or anything that isn't a regular file.
func addPlugins(zw *zip.Writer) error {
	root := core.AppPath("plugins")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !validName(e.Name()) {
			continue // .staging-*, .backup-*: installs in progress
		}
		dir := filepath.Join(root, e.Name())
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == dir {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if isPluginCacheFile(rel) {
				return nil
			}
			return addFile(zw, dirPlugins+e.Name()+"/"+rel, p)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func addBytes(zw *zip.Writer, name string, b []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func addFile(zw *zip.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: st.ModTime()})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, f)
	return err
}
