// Package backup exports the whole server configuration to one compressed
// file and imports it back, for moving a server to another machine or
// restoring it.
//
// A backup (.mycelium) is a ZIP, optionally sealed with a passphrase
// (crypt.go):
//
//	manifest.json      format, core version, creation time, host
//	config.json        data/config.json as stored (never the env overlay)
//	mycelium.db        compacted snapshot of the database
//	avatars/…          profile pictures
//	wireproxy/…        WireGuard configurations of the VPN exits
//	plugins/<dir>/…    installed plugins, without their regenerated caches
//
// Caches are left out (image cache, logo analysis, plugin *_cache.json and
// *_index.json, Redis), and so are logs, offline downloads (temporary files)
// and the web app build, which the server downloads again.
//
// Import happens in two steps: Stage unpacks an uploaded file and describes
// what it holds (Summary), item by item; Schedule records the admin's choice
// and the server restarts; ApplyPending applies it at the next boot, before
// plugins, VPN exits and keys are loaded, so nothing is swapped while running.
package backup

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mycelium/internal/core"
)

// Ext is the backup file extension.
const Ext = ".mycelium"

const (
	formatName    = "mycelium-backup"
	formatVersion = 1
)

// Manifest is manifest.json.
type Manifest struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	CoreVersion   string `json:"core_version"`
	CreatedAt     int64  `json:"created_at"`
	Hostname      string `json:"hostname"`
}

// Archive entry names.
const (
	entryManifest = "manifest.json"
	entryConfig   = "config.json"
	entryDB       = "mycelium.db"
	dirAvatars    = "avatars/"
	dirWireproxy  = "wireproxy/"
	dirPlugins    = "plugins/"
)

func backupsDir() string { return core.AppPath("data", "backups") }
func stagedDir() string  { return filepath.Join(backupsDir(), "staged") }
func stagedArchive() string {
	return filepath.Join(stagedDir(), "archive.zip")
}
func stagedDB() string      { return filepath.Join(stagedDir(), "mycelium.db") }
func stagedSummary() string { return filepath.Join(stagedDir(), "summary.json") }
func pendingFile() string   { return filepath.Join(stagedDir(), "pending.json") }
func lastResultFile() string {
	return filepath.Join(backupsDir(), "last-import.json")
}

// Section keys.
const (
	SecSettings = "settings"
	SecIdentity = "identity"
	SecVPN      = "vpn"
	SecPlugins  = "plugins"
	SecProfiles = "profiles"
	SecDevices  = "devices"
)

// Items of the sections that aren't lists of things.
const (
	itemAll           = "all"
	itemAdminPassword = "admin_password"
	itemServerKeys    = "server_keys"
)

// sharedPluginDir holds modules preloaded into every plugin: not a plugin
// itself (no manifest), but part of the installed set.
const sharedPluginDir = "shared"

// skippedKeys are settings that describe this machine's files, not
// configuration: carried over they would lie (the web app is not in the
// backup, so its installed tag must not be either).
var skippedKeys = map[string]bool{
	"pileus_web_installed_tag": true,
}

// classifyKey says which section and item a config.json key belongs to;
// section "" means it is never exported.
func classifyKey(k string) (section, item string) {
	switch {
	case skippedKeys[k]:
		return "", ""
	case k == "master_admin_hash":
		return SecIdentity, itemAdminPassword
	case k == "pileus_jwt_secret", k == "pileus_grpc_tls_cert", k == "pileus_grpc_tls_key":
		return SecIdentity, itemServerKeys
	case k == "egress_profiles", k == "vpn_proxy_url", k == "egress_ipv6":
		return SecVPN, itemAll
	case strings.HasPrefix(k, "lua:"):
		rest := k[len("lua:"):]
		if id, _, ok := strings.Cut(rest, ":"); ok && id != "" {
			return SecPlugins, id
		}
	case strings.HasPrefix(k, "plugin_") && strings.HasSuffix(k, "_disabled"):
		id := strings.TrimSuffix(strings.TrimPrefix(k, "plugin_"), "_disabled")
		if id != "" {
			return SecPlugins, id
		}
	}
	return SecSettings, k
}

// isPluginCacheFile reports whether a plugin file (path relative to its
// directory, slash-separated) is a regenerated cache: same naming convention
// as the dashboard's "wipe plugin data" (api.pluginCacheFiles), at any depth.
func isPluginCacheFile(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasSuffix(base, "_cache.json") || strings.HasSuffix(base, "_index.json")
}

// validName accepts a single path element of a backup entry: no separators,
// no dot-files, nothing that climbs out of its directory.
func validName(s string) bool {
	return s != "" && !strings.HasPrefix(s, ".") && !strings.ContainsAny(s, `/\`) && s != ".."
}

// keepNewest removes all but the n newest files matching pattern.
func keepNewest(pattern string, n int) {
	m, _ := filepath.Glob(pattern)
	if len(m) <= n {
		return
	}
	type f struct {
		p string
		t int64
	}
	var fs []f
	for _, p := range m {
		if st, err := os.Stat(p); err == nil {
			fs = append(fs, f{p, st.ModTime().UnixNano()})
		}
	}
	slices.SortFunc(fs, func(a, b f) int { return cmp.Compare(b.t, a.t) }) // newest first
	for _, x := range fs[min(n, len(fs)):] {
		os.Remove(x.p)
	}
}
