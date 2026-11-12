package backup

import (
	"archive/zip"
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"

	_ "modernc.org/sqlite"
)

// Extraction limits: generous for a server's whole configuration, but a
// crafted archive can't fill the disk.
const (
	maxEntrySize = 1 << 30
	maxTotalSize = 4 << 30
)

// Item statuses, compared with what this server has now.
const (
	StatusNew     = "new"     // not on this server
	StatusReplace = "replace" // present here, the backup's copy replaces it
	StatusSame    = "same"    // identical
)

// Summary describes a staged backup, section by section.
type Summary struct {
	Manifest  Manifest  `json:"manifest"`
	Encrypted bool      `json:"encrypted"`
	Bytes     int64     `json:"bytes"` // unpacked size
	Warnings  []string  `json:"warnings,omitempty"`
	Sections  []Section `json:"sections"`
}

// Section is one group of importable items.
type Section struct {
	Key   string `json:"key"`
	Items []Item `json:"items"`
}

// Item is one importable thing. Which detail fields are set depends on the
// section.
type Item struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Warning string `json:"warning,omitempty"`

	Version          string   `json:"version,omitempty"`           // plugins
	InstalledVersion string   `json:"installed_version,omitempty"` // plugins
	Files            int      `json:"files,omitempty"`             // plugins, vpn
	Bytes            int64    `json:"bytes,omitempty"`             // plugins
	Settings         int      `json:"settings,omitempty"`          // plugins
	Secrets          int      `json:"secrets,omitempty"`           // plugins, profiles
	History          int      `json:"history,omitempty"`           // profiles
	List             int      `json:"list,omitempty"`              // profiles
	PIN              bool     `json:"pin,omitempty"`               // profiles
	Avatar           bool     `json:"avatar,omitempty"`            // profiles
	Count            int      `json:"count,omitempty"`             // devices
	Names            []string `json:"names,omitempty"`             // vpn
}

// mu serializes staging, scheduling and discarding: they all rewrite the
// staging directory.
var mu sync.Mutex

func stagedFiles() string { return filepath.Join(stagedDir(), "files") }

// Stage unpacks an uploaded backup (opened with passphrase if it is sealed)
// into the staging directory, replacing any earlier one, and describes it.
func Stage(src io.Reader, passphrase string) (*Summary, error) {
	mu.Lock()
	defer mu.Unlock()

	if pendingExists() {
		return nil, errors.New("un ripristino è già programmato: riavvia il server per applicarlo o annullalo")
	}
	os.RemoveAll(stagedDir())
	if err := os.MkdirAll(stagedDir(), 0o700); err != nil {
		return nil, err
	}
	sum, err := stage(src, passphrase)
	if err != nil {
		os.RemoveAll(stagedDir())
		return nil, err
	}
	b, _ := json.Marshal(sum)
	if err := os.WriteFile(stagedSummary(), b, 0o600); err != nil {
		os.RemoveAll(stagedDir())
		return nil, err
	}
	return sum, nil
}

func stage(src io.Reader, passphrase string) (*Summary, error) {
	br := bufio.NewReader(src)
	head, _ := br.Peek(len(encMagic))
	encrypted := isEncrypted(head)

	arch, err := os.OpenFile(stagedArchive(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if encrypted {
		if passphrase == "" {
			arch.Close()
			return nil, ErrNeedPassphrase
		}
		br.Discard(len(encMagic))
		err = decryptStream(arch, br, passphrase)
	} else {
		_, err = io.Copy(arch, br)
	}
	if cerr := arch.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	defer os.Remove(stagedArchive()) // everything is unpacked below

	zr, err := zip.OpenReader(stagedArchive())
	if err != nil {
		return nil, errors.New("il file non è un backup di mycelium")
	}
	defer zr.Close()

	man, err := readManifest(&zr.Reader)
	if err != nil {
		return nil, err
	}
	total, err := extractArchive(&zr.Reader)
	if err != nil {
		return nil, err
	}

	sum := &Summary{Manifest: man, Encrypted: encrypted, Bytes: total}
	if core.IsComparableVersion(man.CoreVersion) && core.IsComparableVersion(core.Version) &&
		core.IsNewerVersion(man.CoreVersion, core.Version) {
		sum.Warnings = append(sum.Warnings, fmt.Sprintf("backup creato con mycelium %s, più recente di questo server (%s): aggiornalo prima di importare", man.CoreVersion, core.Version))
	}
	if err := describe(sum); err != nil {
		return nil, err
	}
	return sum, nil
}

func readManifest(zr *zip.Reader) (Manifest, error) {
	var man Manifest
	f, err := zr.Open(entryManifest)
	if err != nil {
		return man, errors.New("il file non è un backup di mycelium (manca manifest.json)")
	}
	defer f.Close()
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&man); err != nil || man.Format != formatName {
		return man, errors.New("il file non è un backup di mycelium")
	}
	if man.FormatVersion > formatVersion {
		return man, fmt.Errorf("backup in un formato più recente (%d): aggiorna mycelium per importarlo", man.FormatVersion)
	}
	return man, nil
}

// extractArchive unpacks the entries this format knows into stagedFiles()
// (the database to stagedDB()), refusing anything that would land outside.
func extractArchive(zr *zip.Reader) (int64, error) {
	root := filepath.Clean(stagedFiles()) + string(os.PathSeparator)
	var total int64
	for _, f := range zr.File {
		name := f.Name
		var dst string
		switch {
		case name == entryManifest:
			continue
		case name == entryDB:
			dst = stagedDB()
		case name == entryConfig:
			dst = filepath.Join(stagedFiles(), entryConfig)
		case strings.HasPrefix(name, dirAvatars), strings.HasPrefix(name, dirWireproxy), strings.HasPrefix(name, dirPlugins):
			if strings.HasSuffix(name, "/") {
				continue
			}
			parts := strings.Split(name, "/")
			if slices.ContainsFunc(parts, func(p string) bool { return p == "" || p == "." || p == ".." || strings.Contains(p, `\`) }) {
				return total, fmt.Errorf("percorso non valido nel backup: %q", name)
			}
			dst = filepath.Join(stagedFiles(), filepath.FromSlash(name))
			if !strings.HasPrefix(filepath.Clean(dst), root) {
				return total, fmt.Errorf("percorso non valido nel backup: %q", name)
			}
		default:
			continue // unknown entry from a newer format: ignored
		}
		if !f.Mode().IsRegular() {
			continue
		}
		if f.UncompressedSize64 > maxEntrySize || total+int64(f.UncompressedSize64) > maxTotalSize {
			return total, errors.New("backup troppo grande una volta estratto")
		}
		n, err := extractEntry(f, dst)
		total += n
		if err != nil {
			return total, fmt.Errorf("%s: %w", name, err)
		}
	}
	return total, nil
}

func extractEntry(f *zip.File, dst string) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(rc, maxEntrySize+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxEntrySize {
		err = errors.New("file troppo grande")
	}
	return n, err
}

// stagedConfig is the staged config.json (empty when the backup has none).
func stagedConfig() map[string]any {
	cfg := map[string]any{}
	if raw, err := os.ReadFile(filepath.Join(stagedFiles(), entryConfig)); err == nil {
		_ = json.Unmarshal(raw, &cfg)
	}
	return cfg
}

// currentConfig is this server's config.json as stored.
func currentConfig() map[string]any {
	cfg := map[string]any{}
	if raw, err := os.ReadFile(core.AppPath("data", "config.json")); err == nil {
		_ = json.Unmarshal(raw, &cfg)
	}
	return cfg
}

func openStagedDB() (*sql.DB, error) {
	if _, err := os.Stat(stagedDB()); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", "file:"+stagedDB()+"?mode=ro")
}

// stagedPlugin is one plugin directory in the staged files.
type stagedPlugin struct {
	ID, Name, Version, Dir string
	Manifest               engine.LuaManifest
	Shared                 bool
}

// stagedPlugins lists the staged plugin directories that hold a valid
// manifest, plus the shared modules.
func stagedPlugins() []stagedPlugin {
	root := filepath.Join(stagedFiles(), "plugins")
	entries, _ := os.ReadDir(root)
	var out []stagedPlugin
	for _, e := range entries {
		if !e.IsDir() || !validName(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if e.Name() == sharedPluginDir {
			out = append(out, stagedPlugin{ID: sharedPluginDir, Name: sharedPluginDir, Dir: dir, Shared: true})
			continue
		}
		mf, err := engine.ReadLuaManifest(dir)
		if err != nil || mf.ID == sharedPluginDir {
			continue
		}
		out = append(out, stagedPlugin{ID: mf.ID, Name: mf.Name, Version: mf.Version, Dir: dir, Manifest: mf})
	}
	return out
}

// installedPluginDirs maps each installed plugin id to its directory.
func installedPluginDirs() map[string]string {
	root := core.AppPath("plugins")
	entries, _ := os.ReadDir(root)
	out := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() || !validName(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if e.Name() == sharedPluginDir {
			out[sharedPluginDir] = dir
			continue
		}
		if mf, err := engine.ReadLuaManifest(dir); err == nil {
			out[mf.ID] = dir
		}
	}
	return out
}

func describe(sum *Summary) error {
	cfg, cur := stagedConfig(), currentConfig()
	keyStatus := func(k string) string {
		v, ok := cur[k]
		switch {
		case !ok:
			return StatusNew
		case reflect.DeepEqual(v, cfg[k]):
			return StatusSame
		}
		return StatusReplace
	}
	groupStatus := func(keys []string) string {
		st := StatusSame
		for _, k := range keys {
			switch keyStatus(k) {
			case StatusReplace:
				return StatusReplace
			case StatusNew:
				st = StatusNew
			}
		}
		return st
	}

	// config.json keys by section.
	byItem := map[string]map[string][]string{}
	for k := range cfg {
		sec, item := classifyKey(k)
		if sec == "" {
			continue
		}
		if byItem[sec] == nil {
			byItem[sec] = map[string][]string{}
		}
		byItem[sec][item] = append(byItem[sec][item], k)
	}

	db, err := openStagedDB()
	if err == nil {
		defer db.Close()
	}
	count := func(q string, args ...any) int {
		if db == nil {
			return 0
		}
		var n int
		_ = db.QueryRow(q, args...).Scan(&n)
		return n
	}

	// Settings, one item per key.
	var settings Section
	settings.Key = SecSettings
	for k := range byItem[SecSettings] {
		settings.Items = append(settings.Items, Item{ID: k, Label: k, Status: keyStatus(k)})
	}
	slices.SortFunc(settings.Items, func(a, b Item) int { return strings.Compare(a.ID, b.ID) })

	// Identity.
	identity := Section{Key: SecIdentity}
	if keys := byItem[SecIdentity][itemAdminPassword]; len(keys) > 0 {
		identity.Items = append(identity.Items, Item{ID: itemAdminPassword, Label: itemAdminPassword, Status: groupStatus(keys)})
	}
	if keys := byItem[SecIdentity][itemServerKeys]; len(keys) > 0 {
		identity.Items = append(identity.Items, Item{ID: itemServerKeys, Label: itemServerKeys, Status: groupStatus(keys)})
	}

	// VPN: the exit registry and its WireGuard files go together.
	vpn := Section{Key: SecVPN}
	confs, _ := filepath.Glob(filepath.Join(stagedFiles(), "wireproxy", "*.conf"))
	if keys := byItem[SecVPN][itemAll]; len(keys) > 0 || len(confs) > 0 {
		it := Item{ID: itemAll, Label: itemAll, Status: groupStatus(keys), Files: len(confs)}
		if raw, ok := cfg["egress_profiles"].(string); ok {
			var list []struct {
				Name string `json:"name"`
			}
			if json.Unmarshal([]byte(raw), &list) == nil {
				for _, p := range list {
					it.Names = append(it.Names, p.Name)
				}
			}
		}
		if it.Status == StatusSame && len(confs) > 0 {
			it.Status = StatusReplace // the files themselves aren't compared
		}
		vpn.Items = append(vpn.Items, it)
	}

	// Plugins.
	installed := installedPluginDirs()
	plugins := Section{Key: SecPlugins}
	for _, p := range stagedPlugins() {
		it := Item{ID: p.ID, Label: p.Name, Version: p.Version, Status: StatusNew}
		if p.Name == "" {
			it.Label = p.ID
		}
		if dir, ok := installed[p.ID]; ok {
			it.Status = StatusReplace
			if mf, err := engine.ReadLuaManifest(dir); err == nil {
				it.InstalledVersion = mf.Version
			}
		}
		_ = filepath.WalkDir(p.Dir, func(_ string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				it.Files++
				if info, e := d.Info(); e == nil {
					it.Bytes += info.Size()
				}
			}
			return nil
		})
		it.Settings = len(byItem[SecPlugins][p.ID])
		it.Secrets = count(`SELECT COUNT(*) FROM plugin_secrets WHERE plugin_id=?`, p.ID)
		if !p.Shared {
			if err := engine.CheckMinCore(p.Manifest); err != nil {
				it.Warning = err.Error()
			}
		}
		plugins.Items = append(plugins.Items, it)
	}

	// Profiles.
	profiles := Section{Key: SecProfiles}
	if db != nil {
		rows, err := db.Query(`SELECT profile_id, name FROM pileus_profiles ORDER BY created_at, name`)
		if err == nil {
			for rows.Next() {
				var it Item
				if rows.Scan(&it.ID, &it.Label) != nil {
					continue
				}
				profiles.Items = append(profiles.Items, it)
			}
			rows.Close()
		}
		for i := range profiles.Items {
			it := &profiles.Items[i]
			it.Status = StatusNew
			if managers.DB != nil {
				var one int
				if managers.DB.QueryRow(`SELECT 1 FROM pileus_profiles WHERE profile_id=?`, it.ID).Scan(&one) == nil {
					it.Status = StatusReplace
				}
			}
			it.History = count(`SELECT COUNT(*) FROM watch_history WHERE client_id=?`, it.ID)
			it.List = count(`SELECT COUNT(*) FROM my_list WHERE owner_id=?`, it.ID)
			it.Secrets = count(`SELECT COUNT(*) FROM plugin_secrets WHERE profile_id=?`, it.ID)
			it.PIN = count(`SELECT COUNT(*) FROM pileus_profiles WHERE profile_id=? AND access_pin_hash IS NOT NULL AND access_pin_hash != ''`, it.ID) > 0
			if _, err := os.Stat(filepath.Join(stagedFiles(), "avatars", it.ID+".jpg")); err == nil {
				it.Avatar = true
			}
		}
	}

	// Devices: all or nothing.
	devices := Section{Key: SecDevices}
	n := count(`SELECT COUNT(*) FROM pileus_devices WHERE revoked_at IS NULL`)
	if n == 0 {
		n = count(`SELECT COUNT(*) FROM pileus_devices`) // backup older than revocation
	}
	if n > 0 {
		devices.Items = append(devices.Items, Item{ID: itemAll, Label: itemAll, Status: StatusNew, Count: n})
	}

	for _, s := range []Section{plugins, profiles, devices, vpn, settings, identity} {
		if len(s.Items) > 0 {
			sum.Sections = append(sum.Sections, s)
		}
	}
	return nil
}
