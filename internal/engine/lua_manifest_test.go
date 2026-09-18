package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func hasField(fields []LuaSettingField, id string) bool {
	for _, f := range fields {
		if f.ID == id {
			return true
		}
	}
	return false
}

// A manifest with no network flag is VPN-routed by default (DirectEgress false),
// so RequireProxy ends up true for it everywhere.
func TestReadLuaManifest_DefaultIsVPNRouted(t *testing.T) {
	mf, err := ReadLuaManifest(writeManifest(t, "id: p\nname: P\n"))
	if err != nil {
		t.Fatal(err)
	}
	if mf.DirectEgress {
		t.Errorf("DirectEgress = true for a manifest with no flag, want false (VPN is the default)")
	}
}

func TestReadLuaManifest_DirectEgressParsed(t *testing.T) {
	mf, err := ReadLuaManifest(writeManifest(t, "id: p\nname: P\ndirect_egress: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !mf.DirectEgress {
		t.Errorf("DirectEgress = false, want true")
	}
}

// vpn_optional injects the synthetic vpn_enabled toggle only when the plugin is
// also direct_egress — on a VPN-routed plugin the toggle would be a no-op.
func TestReadLuaManifest_VPNOptionalInjection(t *testing.T) {
	withDirect, err := ReadLuaManifest(writeManifest(t, "id: p\nname: P\ndirect_egress: true\nvpn_optional: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !hasField(withDirect.Settings.Global, VPNOptInSettingID) {
		t.Errorf("direct_egress + vpn_optional: expected a %q setting to be injected", VPNOptInSettingID)
	}

	optionalOnly, err := ReadLuaManifest(writeManifest(t, "id: p\nname: P\nvpn_optional: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if hasField(optionalOnly.Settings.Global, VPNOptInSettingID) {
		t.Errorf("vpn_optional without direct_egress: %q must NOT be injected (VPN already covers everything)", VPNOptInSettingID)
	}
}

// mf.ID (manifest.yaml's `id:`) is never sanitized by the YAML parser and
// ends up interpolated into HTML attributes in web/static/admin.js
// (buildCard) plus filesystem/Redis/setting keys throughout this package. A
// hostile ZIP upload with a crafted id must be rejected at load time,
// regardless of what the frontend does with it (defense in depth).
func TestReadLuaManifest_RejectsUnsafeID(t *testing.T) {
	unsafe := []string{
		`plugin"onmouseover=alert(1)`, // HTML attribute breakout
		`<script>alert(1)</script>`,
		"plugin id", // space
		"../../etc", // path traversal-ish
		"Plugin",    // uppercase not allowed
		".leading",  // leading punctuation
		"trailing.", // trailing punctuation
		"",          // empty (covered separately below too)
	}
	for _, id := range unsafe {
		body := "id: " + strconv.Quote(id) + "\nname: P\n"
		if _, err := ReadLuaManifest(writeManifest(t, body)); err == nil {
			t.Errorf("id %q: expected ReadLuaManifest to reject it, got no error", id)
		}
	}
}

// Legitimate ids in use by real plugins in this repo (plugins/*/manifest.yaml)
// — notably the dotted "vix.movie" / "vix.series" form — must keep working.
func TestReadLuaManifest_AcceptsLegitimateID(t *testing.T) {
	legit := []string{"animeunity", "jellyfin", "vix.movie", "vix.series", "cdnlivetv", "sport", "watchfooty", "a", "plugin-name_1.2"}
	for _, id := range legit {
		body := "id: " + strconv.Quote(id) + "\nname: P\n"
		mf, err := ReadLuaManifest(writeManifest(t, body))
		if err != nil {
			t.Errorf("id %q: expected ReadLuaManifest to accept it, got error: %v", id, err)
			continue
		}
		if mf.ID != id {
			t.Errorf("id %q: mf.ID = %q, want unchanged", id, mf.ID)
		}
	}
}
