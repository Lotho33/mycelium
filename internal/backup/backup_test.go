package backup

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

func TestCryptRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, encChunk - 1, encChunk, encChunk + 1, 3*encChunk + 17} {
		plain := make([]byte, size)
		rand.Read(plain)
		var sealed bytes.Buffer
		if err := encryptStream(&sealed, bytes.NewReader(plain), "segreta"); err != nil {
			t.Fatalf("size %d: encrypt: %v", size, err)
		}
		if !isEncrypted(sealed.Bytes()) {
			t.Fatalf("size %d: magic missing", size)
		}
		body := sealed.Bytes()[len(encMagic):]

		var out bytes.Buffer
		if err := decryptStream(&out, bytes.NewReader(body), "segreta"); err != nil {
			t.Fatalf("size %d: decrypt: %v", size, err)
		}
		if !bytes.Equal(out.Bytes(), plain) {
			t.Fatalf("size %d: round trip differs", size)
		}
		if err := decryptStream(&bytes.Buffer{}, bytes.NewReader(body), "sbagliata"); !errors.Is(err, ErrWrongPassphrase) {
			t.Fatalf("size %d: wrong passphrase: err = %v", size, err)
		}
		// Dropping the last chunk must not decrypt to a shorter archive.
		if size > encChunk {
			cut := saltLen + noncePfx + 4 + encChunk + gcmOverhed
			if err := decryptStream(&bytes.Buffer{}, bytes.NewReader(body[:cut]), "segreta"); err == nil {
				t.Fatalf("size %d: truncated file decrypted", size)
			}
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	b, _ := json.Marshal(cfg)
	write(t, core.AppPath("data", "config.json"), string(b))
}

// newServer points the process at an empty base directory with a fresh
// database and the given config.json, like a freshly installed server.
func newServer(t *testing.T, cfg map[string]any) string {
	t.Helper()
	base := t.TempDir()
	core.BasePath = base
	writeConfig(t, cfg)
	managers.Settings.Load()
	if err := managers.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return base
}

func exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := managers.DB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func countRows(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := managers.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func findSection(s *Summary, key string) *Section {
	for i := range s.Sections {
		if s.Sections[i].Key == key {
			return &s.Sections[i]
		}
	}
	return nil
}

func TestExportStageApply(t *testing.T) {
	oldBase, oldDB := core.BasePath, managers.DB
	t.Cleanup(func() {
		core.BasePath, managers.DB = oldBase, oldDB
		managers.Settings.Load()
	})

	// ── Old server ──
	newServer(t, map[string]any{
		"master_admin_hash":        "old-admin-hash",
		"pileus_jwt_secret":        "old-jwt",
		"pileus_grpc_tls_cert":     "old-cert",
		"pileus_grpc_tls_key":      "old-key",
		"prebuffer_enabled":        "1",
		"server_host":              "10.0.0.5",
		"lua:demo:global:token":    "abc",
		"plugin_demo_disabled":     "false",
		"lua:gone:global:x":        "leftover of an uninstalled plugin",
		"egress_profiles":          `[{"name":"casa","kind":"wireproxy"}]`,
		"pileus_web_installed_tag": "owner/repo@v1",
	})
	exec(t, `INSERT INTO pileus_devices(device_id, label) VALUES('d1','TV')`)
	exec(t, `INSERT INTO pileus_profiles(profile_id, device_id, name, preferences) VALUES('p1','d1','Anna','{"lang":"it"}')`)
	exec(t, `INSERT INTO pileus_profiles(profile_id, device_id, name) VALUES('p2','d1','Bruno')`)
	exec(t, `INSERT INTO watch_history(client_id, provider_id, playable_id, title) VALUES('p1','demo','m1','Film')`)
	exec(t, `INSERT INTO watch_history(client_id, provider_id, playable_id, title) VALUES('p2','demo','m2','Altro')`)
	exec(t, `INSERT INTO my_list(owner_id, plugin_id, media_id, added_at) VALUES('p1','demo','m1',1)`)
	exec(t, `INSERT INTO pileus_profile_trust(device_id, profile_id) VALUES('d1','p1')`)
	exec(t, `INSERT INTO plugin_secrets(plugin_id, profile_id, key, value) VALUES('demo','p1','login','anna'),('demo','p2','login','bruno'),('demo','','global','g')`)
	write(t, core.AppPath("data", "avatars", "p1.jpg"), "jpeg-bytes")
	write(t, core.AppPath("data", "wireproxy", "casa.conf"), "[Interface]\nPrivateKey = x\n")
	write(t, core.AppPath("plugins", "demo", "manifest.yaml"), "id: demo\nname: Demo\nversion: 1.2.0\n")
	write(t, core.AppPath("plugins", "demo", "init.lua"), "return {}\n")
	write(t, core.AppPath("plugins", "demo", "lib", "util.lua"), "return 1\n")
	write(t, core.AppPath("plugins", "demo", "state.json"), `{"keep":true}`)
	write(t, core.AppPath("plugins", "demo", "catalog_cache.json"), "[]")
	write(t, core.AppPath("plugins", "demo", "lib", "titles_index.json"), "[]")
	write(t, core.AppPath("plugins", ".staging-123", "junk"), "x")
	write(t, core.AppPath("data", "imgcache", "a.webp"), "cache")

	var file bytes.Buffer
	if err := Export(&file, "pass phrase"); err != nil {
		t.Fatalf("Export: %v", err)
	}
	exported := file.Bytes()

	// ── New server ──
	newServer(t, map[string]any{
		"master_admin_hash": "new-admin-hash",
		"pileus_jwt_secret": "new-jwt",
		"server_host":       "10.0.0.9",
	})
	exec(t, `INSERT INTO pileus_profiles(profile_id, device_id, name) VALUES('p1','x','Anna vecchia')`)

	if _, err := Stage(bytes.NewReader(exported), ""); !errors.Is(err, ErrNeedPassphrase) {
		t.Fatalf("no passphrase: err = %v", err)
	}
	if _, err := Stage(bytes.NewReader(exported), "wrong"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: err = %v", err)
	}
	if _, err := Stage(bytes.NewReader([]byte("not a backup")), ""); err == nil {
		t.Fatal("garbage staged")
	}
	sum, err := Stage(bytes.NewReader(exported), "pass phrase")
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !sum.Encrypted {
		t.Error("summary: not marked encrypted")
	}

	// Summary content.
	plugins := findSection(sum, SecPlugins)
	if plugins == nil || len(plugins.Items) != 1 {
		t.Fatalf("plugins section = %+v", plugins)
	}
	if p := plugins.Items[0]; p.ID != "demo" || p.Version != "1.2.0" || p.Status != StatusNew || p.Files != 4 || p.Settings != 2 || p.Secrets != 3 {
		t.Errorf("plugin item = %+v", p)
	}
	profiles := findSection(sum, SecProfiles)
	if profiles == nil || len(profiles.Items) != 2 {
		t.Fatalf("profiles section = %+v", profiles)
	}
	for _, p := range profiles.Items {
		switch p.ID {
		case "p1":
			if p.Status != StatusReplace || p.History != 1 || p.List != 1 || !p.Avatar {
				t.Errorf("p1 = %+v", p)
			}
		case "p2":
			if p.Status != StatusNew || p.Avatar {
				t.Errorf("p2 = %+v", p)
			}
		}
	}
	settings := findSection(sum, SecSettings)
	var keys []string
	for _, it := range settings.Items {
		keys = append(keys, it.ID)
		if it.ID == "server_host" && it.Status != StatusReplace {
			t.Errorf("server_host status = %s", it.Status)
		}
	}
	if !slices.Equal(keys, []string{"prebuffer_enabled", "server_host"}) {
		t.Errorf("settings keys = %v", keys)
	}
	if vpn := findSection(sum, SecVPN); vpn == nil || vpn.Items[0].Files != 1 || !slices.Equal(vpn.Items[0].Names, []string{"casa"}) {
		t.Errorf("vpn section = %+v", vpn)
	}
	if id := findSection(sum, SecIdentity); id == nil || len(id.Items) != 2 {
		t.Errorf("identity section = %+v", id)
	}
	if d := findSection(sum, SecDevices); d == nil || d.Items[0].Count != 1 {
		t.Errorf("devices section = %+v", d)
	}

	// Choose: the plugin, only Anna, the devices, one setting, the server keys
	// but not the admin password, the VPN. Unknown ids are dropped.
	safety, err := Schedule(Selection{
		SecPlugins:  {"demo", "nope"},
		SecProfiles: {"p1"},
		SecDevices:  {itemAll},
		SecSettings: {"prebuffer_enabled"},
		SecIdentity: {itemServerKeys},
		SecVPN:      {itemAll},
	})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if SafetyCopyPath(safety) == "" {
		t.Fatalf("safety copy %q missing", safety)
	}
	if _, err := Stage(bytes.NewReader(exported), "pass phrase"); err == nil {
		t.Error("Stage allowed while a restore is scheduled")
	}
	if !CurrentStatus().Pending {
		t.Error("status: not pending")
	}

	// ── Next boot ──
	ApplyPending()

	cfg := currentConfig()
	want := map[string]any{
		"master_admin_hash":     "new-admin-hash", // not chosen
		"pileus_jwt_secret":     "old-jwt",
		"pileus_grpc_tls_key":   "old-key",
		"server_host":           "10.0.0.9", // not chosen
		"prebuffer_enabled":     "1",
		"lua:demo:global:token": "abc",
		"plugin_demo_disabled":  "false",
	}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("config[%s] = %v, want %v", k, cfg[k], v)
		}
	}
	for _, k := range []string{"pileus_web_installed_tag", "lua:gone:global:x"} {
		if _, ok := cfg[k]; ok {
			t.Errorf("config[%s] imported", k)
		}
	}
	if managers.Settings.GetString("pileus_jwt_secret", "") != "old-jwt" {
		t.Error("settings not reloaded after the restore")
	}

	if n := countRows(t, `SELECT COUNT(*) FROM pileus_profiles WHERE profile_id='p1' AND name='Anna' AND preferences='{"lang":"it"}'`); n != 1 {
		t.Error("p1 not replaced by the backup's copy")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM pileus_profiles WHERE profile_id='p2'`); n != 0 {
		t.Error("p2 imported although not chosen")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM watch_history`); n != 1 {
		t.Errorf("watch_history rows = %d, want 1 (only p1)", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM my_list WHERE owner_id='p1'`); n != 1 {
		t.Error("my_list of p1 missing")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM pileus_devices WHERE device_id='d1' AND label='TV'`); n != 1 {
		t.Error("device missing")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM pileus_profile_trust`); n != 1 {
		t.Error("profile trust missing")
	}
	// p1's and the global secret; not p2's (profile not chosen).
	if n := countRows(t, `SELECT COUNT(*) FROM plugin_secrets`); n != 2 {
		t.Errorf("plugin_secrets rows = %d, want 2", n)
	}

	if b, _ := os.ReadFile(core.AppPath("data", "avatars", "p1.jpg")); string(b) != "jpeg-bytes" {
		t.Error("avatar not restored")
	}
	if _, err := os.Stat(core.AppPath("data", "wireproxy", "casa.conf")); err != nil {
		t.Error("wireproxy conf not restored")
	}
	for _, f := range []string{"manifest.yaml", "init.lua", "lib/util.lua", "state.json"} {
		if _, err := os.Stat(core.AppPath("plugins", "demo", f)); err != nil {
			t.Errorf("plugin file %s missing", f)
		}
	}
	for _, f := range []string{"catalog_cache.json", "lib/titles_index.json"} {
		if _, err := os.Stat(core.AppPath("plugins", "demo", f)); err == nil {
			t.Errorf("cache file %s was exported", f)
		}
	}
	if _, err := os.Stat(core.AppPath("plugins", ".staging-123")); err == nil {
		t.Error("staging dir exported")
	}
	if _, err := os.Stat(core.AppPath("data", "imgcache")); err == nil {
		t.Error("image cache exported")
	}

	st := CurrentStatus()
	if st.Staged != nil || st.Pending {
		t.Error("staging not cleaned after the restore")
	}
	if st.Last == nil || st.Last.Imported[SecPlugins] != 1 || st.Last.Imported[SecProfiles] != 1 || len(st.Last.Errors) != 0 {
		t.Errorf("last result = %+v", st.Last)
	}
	// Nothing scheduled: a second boot does nothing.
	ApplyPending()
}

func TestStageRejectsTraversal(t *testing.T) {
	oldBase := core.BasePath
	t.Cleanup(func() { core.BasePath = oldBase })
	core.BasePath = t.TempDir()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	man, _ := json.Marshal(Manifest{Format: formatName, FormatVersion: formatVersion})
	addBytes(zw, entryManifest, man)
	addBytes(zw, "plugins/../../evil.lua", []byte("x"))
	zw.Close()

	if _, err := Stage(&buf, ""); err == nil {
		t.Fatal("archive with ../ staged")
	}
	if _, err := os.Stat(filepath.Join(core.BasePath, "evil.lua")); err == nil {
		t.Fatal("file written outside the staging directory")
	}
}
