package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// Selection is what to import: section key → item ids.
type Selection map[string][]string

func (s Selection) has(sec, item string) bool { return slices.Contains(s[sec], item) }

type pending struct {
	Selection  Selection `json:"selection"`
	SafetyCopy string    `json:"safety_copy"`
	At         int64     `json:"at"`
}

// Result is what the last import applied (last-import.json).
type Result struct {
	AppliedAt  int64            `json:"applied_at"`
	Source     Manifest         `json:"source"`
	SafetyCopy string           `json:"safety_copy,omitempty"`
	Imported   map[string]int   `json:"imported"` // section → items applied
	Rows       map[string]int64 `json:"rows,omitempty"`
	Errors     []string         `json:"errors,omitempty"`
}

// Status is the dashboard's view of the import state.
type Status struct {
	Staged  *Summary `json:"staged,omitempty"`
	Pending bool     `json:"pending"`
	Last    *Result  `json:"last,omitempty"`
}

func pendingExists() bool {
	_, err := os.Stat(pendingFile())
	return err == nil
}

func readSummary() (*Summary, error) {
	raw, err := os.ReadFile(stagedSummary())
	if err != nil {
		return nil, err
	}
	var s Summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// CurrentStatus reports the staged backup, whether it is scheduled, and the
// outcome of the last import.
func CurrentStatus() Status {
	mu.Lock()
	defer mu.Unlock()
	var st Status
	st.Staged, _ = readSummary()
	st.Pending = st.Staged != nil && pendingExists()
	if raw, err := os.ReadFile(lastResultFile()); err == nil {
		var r Result
		if json.Unmarshal(raw, &r) == nil {
			st.Last = &r
		}
	}
	return st
}

// Discard drops the staged backup (and its schedule, if any).
func Discard() error {
	mu.Lock()
	defer mu.Unlock()
	return os.RemoveAll(stagedDir())
}

// Schedule validates sel against the staged backup, saves a safety copy of
// the current configuration and records the choice for the next boot. It
// returns the safety copy's file name. The caller restarts the server.
func Schedule(sel Selection) (string, error) {
	mu.Lock()
	defer mu.Unlock()

	sum, err := readSummary()
	if err != nil {
		return "", errors.New("nessun backup caricato")
	}
	clean := Selection{}
	n := 0
	for _, s := range sum.Sections {
		for _, it := range s.Items {
			if sel.has(s.Key, it.ID) {
				clean[s.Key] = append(clean[s.Key], it.ID)
				n++
			}
		}
	}
	if n == 0 {
		return "", errors.New("non hai scelto niente da importare")
	}

	// A copy of what is about to be overwritten, so an import can be undone
	// by importing it back.
	safety := "pre-import-" + time.Now().Format("20060102-150405") + Ext
	if err := ExportToFile(filepath.Join(backupsDir(), safety), ""); err != nil {
		return "", fmt.Errorf("copia di sicurezza della configurazione attuale non riuscita: %w", err)
	}
	keepNewest(filepath.Join(backupsDir(), "pre-import-*"+Ext), 3)

	b, _ := json.Marshal(pending{Selection: clean, SafetyCopy: safety, At: time.Now().Unix()})
	if err := os.WriteFile(pendingFile(), b, 0o600); err != nil {
		return "", err
	}
	return safety, nil
}

// SafetyCopyPath is the path of a safety copy by its file name ("" when the
// name isn't one).
func SafetyCopyPath(name string) string {
	if !validName(name) || !strings.HasPrefix(name, "pre-import-") || !strings.HasSuffix(name, Ext) {
		return ""
	}
	p := filepath.Join(backupsDir(), name)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// ApplyPending applies a scheduled import. It runs at boot, after the
// settings and the database are open and before anything reads them
// (VPN exits, plugins, keys); it reloads the settings itself. Without a
// schedule it does nothing. Failures are logged and recorded in the result:
// the server still starts.
func ApplyPending() {
	mu.Lock()
	defer mu.Unlock()

	raw, err := os.ReadFile(pendingFile())
	if err != nil {
		return
	}
	var p pending
	sum, serr := readSummary()
	if json.Unmarshal(raw, &p) != nil || serr != nil {
		log.Printf("[backup] ripristino programmato illeggibile: annullato")
		os.RemoveAll(stagedDir())
		return
	}
	log.Printf("[backup] applico il ripristino del backup del %s (%s)", time.Unix(sum.Manifest.CreatedAt, 0).Format("2006-01-02 15:04"), sum.Manifest.Hostname)

	res := apply(p.Selection)
	res.AppliedAt = time.Now().Unix()
	res.Source = sum.Manifest
	res.SafetyCopy = p.SafetyCopy
	for _, e := range res.Errors {
		log.Printf("[backup] ripristino: %s", e)
	}
	log.Printf("[backup] ripristino completato: %v, %d errori", res.Imported, len(res.Errors))

	b, _ := json.MarshalIndent(res, "", "  ")
	_ = os.WriteFile(lastResultFile(), b, 0o600)
	os.RemoveAll(stagedDir())
	managers.Settings.Load()
}

func apply(sel Selection) Result {
	res := Result{Imported: map[string]int{}}
	fail := func(format string, a ...any) { res.Errors = append(res.Errors, fmt.Sprintf(format, a...)) }

	plugins := sel[SecPlugins]
	profiles := sel[SecProfiles]
	devices := sel.has(SecDevices, itemAll)

	// 1. config.json keys.
	cfg := stagedConfig()
	keys := map[string]any{}
	for k, v := range cfg {
		sec, item := classifyKey(k)
		if sec != "" && sel.has(sec, item) {
			keys[k] = v
		}
	}
	if len(keys) > 0 {
		if err := managers.Settings.SaveInternal(keys); err != nil {
			fail("impostazioni: %v", err)
		}
	}
	res.Imported[SecSettings] = len(sel[SecSettings])
	res.Imported[SecIdentity] = len(sel[SecIdentity])

	// 2. Database rows.
	if managers.DB != nil {
		if _, err := os.Stat(stagedDB()); err == nil {
			copies := dbCopies(plugins, profiles, devices)
			if len(copies) > 0 {
				rows, err := managers.DB.ImportTables(context.Background(), stagedDB(), copies)
				if err != nil {
					fail("database: %v", err)
				} else {
					res.Rows = rows
				}
			}
		}
	}

	// 3. Profile pictures.
	for _, id := range profiles {
		name := id + ".jpg"
		if !validName(name) {
			continue
		}
		src := filepath.Join(stagedFiles(), "avatars", name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := copyFile(src, core.AppPath("data", "avatars", name), 0o600); err != nil {
			fail("foto del profilo %s: %v", id, err)
		}
	}
	res.Imported[SecProfiles] = len(profiles)
	if devices {
		res.Imported[SecDevices] = 1
	}

	// 4. WireGuard files of the VPN exits.
	if sel.has(SecVPN, itemAll) {
		confs, _ := filepath.Glob(filepath.Join(stagedFiles(), "wireproxy", "*.conf"))
		for _, c := range confs {
			if err := copyFile(c, core.AppPath("data", "wireproxy", filepath.Base(c)), 0o600); err != nil {
				fail("VPN %s: %v", filepath.Base(c), err)
			}
		}
		res.Imported[SecVPN] = 1
	}

	// 5. Plugins: the backup's copy replaces the installed one entirely.
	installed := installedPluginDirs()
	for _, sp := range stagedPlugins() {
		if !slices.Contains(plugins, sp.ID) {
			continue
		}
		if err := replacePlugin(sp, installed[sp.ID]); err != nil {
			fail("plugin %s: %v", sp.ID, err)
			continue
		}
		res.Imported[SecPlugins]++
	}
	return res
}

// dbCopies maps the selection onto table copies. Plugin secrets follow
// their plugin; a per-profile secret also needs its profile, unless that
// profile isn't in the backup (then it belongs to nobody being chosen).
func dbCopies(plugins, profiles []string, devices bool) []managers.TableCopy {
	in := func(col string, ids []string) (string, []any) {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		return col + " IN (" + ph + ")", args
	}
	var out []managers.TableCopy
	if len(profiles) > 0 {
		w, a := in("profile_id", profiles)
		out = append(out, managers.TableCopy{Table: "pileus_profiles", Where: w, Args: a})
		w, a = in("client_id", profiles)
		out = append(out, managers.TableCopy{Table: "watch_history", Where: w, Args: a})
		w, a = in("owner_id", profiles)
		out = append(out, managers.TableCopy{Table: "my_list", Where: w, Args: a})
	}
	if devices {
		const devs = `(SELECT device_id FROM bk.pileus_devices)`
		out = append(out,
			managers.TableCopy{Table: "pileus_devices"},
			// History and lists of clients that use no profile.
			managers.TableCopy{Table: "watch_history", Where: "client_id IN " + devs},
			managers.TableCopy{Table: "my_list", Where: "owner_id IN " + devs},
		)
		if len(profiles) > 0 {
			w, a := in("profile_id", profiles)
			out = append(out, managers.TableCopy{Table: "pileus_profile_trust", Where: w + " AND device_id IN " + devs, Args: a})
		}
	}
	if len(plugins) > 0 {
		w, a := in("plugin_id", plugins)
		cond := w + " AND (profile_id NOT IN (SELECT profile_id FROM bk.pileus_profiles)"
		if len(profiles) > 0 {
			pw, pa := in("profile_id", profiles)
			cond += " OR " + pw
			a = append(a, pa...)
		}
		cond += ")"
		out = append(out, managers.TableCopy{Table: "plugin_secrets", Where: cond, Args: a})
	}
	return out
}

// replacePlugin puts the staged plugin in place of the installed one (if
// any). The new copy is written next to its destination first, so a failed
// copy leaves the installed plugin untouched.
func replacePlugin(sp stagedPlugin, installedDir string) error {
	root := core.AppPath("plugins")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	dest := installedDir
	if dest == "" {
		dest = filepath.Join(root, sp.ID)
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("la cartella plugins/%s esiste già e non contiene questo plugin", sp.ID)
		}
	}
	tmp, err := os.MkdirTemp(root, ".restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(sp.Dir, tmp); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// copyTree copies the regular files under src into dst (plugin files are
// 0644, like an upload's).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(p, target, 0o644)
	})
}

// copyFile copies src to dst through a temporary file next to it.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Exported ids of the identity items, for callers that treat them apart
// (the setup page restores the admin password before the restart).
const (
	SectionIdentity   = SecIdentity
	ItemAdminPassword = itemAdminPassword
)

// Has reports whether sel picks item of section.
func (s Selection) Has(section, item string) bool { return s.has(section, item) }

// StagedAdminHash is the admin password hash held by the staged backup, ""
// when there is none (or it isn't a string).
func StagedAdminHash() string {
	mu.Lock()
	defer mu.Unlock()
	v, _ := stagedConfig()["master_admin_hash"].(string)
	return v
}
