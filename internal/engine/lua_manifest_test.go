package engine

import (
	"os"
	"path/filepath"
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
