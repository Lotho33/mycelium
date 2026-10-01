package managers

import (
	"encoding/json"
	"os"
	"testing"
)

// master_admin_hash must stay out of the generic allowlist: only
// SaveInternal (setup, and the password change after checking the current
// password) may write it.
func TestIsAllowedKey_RejectsMasterAdminHash(t *testing.T) {
	if isAllowedKey("master_admin_hash") {
		t.Fatal("master_admin_hash must not be in the generic settings allowlist")
	}
}

// Nor may the master secret (pileus_jwt_secret) or the gRPC TLS key/cert be
// writable through the generic endpoint; pileus_web_repo stays allowed by
// exact name.
func TestIsAllowedKey_PileusKeys(t *testing.T) {
	for _, k := range []string{"pileus_jwt_secret", "pileus_grpc_tls_key", "pileus_grpc_tls_cert", "pileus_grpc_tls", "pileus_web_repo_x"} {
		if isAllowedKey(k) {
			t.Errorf("%s must not be in the generic settings allowlist", k)
		}
	}
	if !isAllowedKey("pileus_web_repo") {
		t.Error("pileus_web_repo must stay writable from the dashboard")
	}
}

// The removed optional HTTPS listener / mDNS responder keys stay out of the
// allowlist, and so do the TLS cert/key settings left in old config files.
func TestIsAllowedKey_RemovedHttpsAndMDNS(t *testing.T) {
	for _, k := range []string{"server_https", "server_https_port", "mdns_enabled", "mdns_hostname",
		"mycelium_web_tls_cert", "mycelium_web_tls_key"} {
		if isAllowedKey(k) {
			t.Errorf("%s must not be in the generic settings allowlist", k)
		}
	}
}

func TestSave_RejectsMasterAdminHash(t *testing.T) {
	withTempSettings(t)

	err := Settings.Save(map[string]any{"master_admin_hash": "$2a$10$whatever"})
	if err == nil {
		t.Fatal("Save must reject master_admin_hash")
	}
	if Settings.MasterAdminHash() != "" {
		t.Fatal("master_admin_hash must not have been written")
	}
}

func TestSave_MixedPayloadWithMasterAdminHashRejectsWholeCall(t *testing.T) {
	withTempSettings(t)

	// A caller sneaking master_admin_hash in alongside an otherwise-allowed
	// key must not get either one written.
	err := Settings.Save(map[string]any{
		"server_port":       "8080",
		"master_admin_hash": "$2a$10$whatever",
	})
	if err == nil {
		t.Fatal("Save must reject the whole payload when it contains a disallowed key")
	}
	if Settings.GetString("server_port", "") != "" {
		t.Fatal("server_port must not have been written when the payload was rejected")
	}
}

func TestSaveInternal_BypassesAllowlist(t *testing.T) {
	withTempSettings(t)

	if err := Settings.SaveInternal(map[string]any{"master_admin_hash": "$2a$10$whatever"}); err != nil {
		t.Fatalf("SaveInternal: %v", err)
	}
	if got := Settings.MasterAdminHash(); got != "$2a$10$whatever" {
		t.Fatalf("MasterAdminHash() = %q; want the value written via SaveInternal", got)
	}
}

// Dashboard checkboxes write "1"/"0": GetBool must read them.
func TestGetBool_AcceptsTheDashboardCheckboxConvention(t *testing.T) {
	withTempSettings(t)

	if err := Settings.SaveInternal(map[string]any{"some_flag": "1"}); err != nil {
		t.Fatalf("SaveInternal: %v", err)
	}
	if !Settings.GetBool("some_flag", false) {
		t.Fatal(`GetBool must treat "1" (the dashboard checkbox convention) as true`)
	}

	if err := Settings.SaveInternal(map[string]any{"some_flag": "0"}); err != nil {
		t.Fatalf("SaveInternal: %v", err)
	}
	if Settings.GetBool("some_flag", true) {
		t.Fatal(`GetBool must treat "0" as false even when def is true`)
	}

	if err := Settings.SaveInternal(map[string]any{"legacy_flag": "true"}); err != nil {
		t.Fatalf("SaveInternal: %v", err)
	}
	if !Settings.GetBool("legacy_flag", false) {
		t.Fatal(`GetBool must still accept the literal "true" (env-var/legacy convention)`)
	}
}

func TestGetBool_FallsBackToDefaultWhenUnset(t *testing.T) {
	withTempSettings(t)

	if !Settings.GetBool("never_set", true) {
		t.Fatal("GetBool must return def=true for a key that was never set")
	}
	if Settings.GetBool("never_set", false) {
		t.Fatal("GetBool must return def=false for a key that was never set")
	}
}

func TestResetAll_ClearsFileAndInMemoryData(t *testing.T) {
	withTempSettings(t)

	if err := Settings.SaveInternal(map[string]any{
		"master_admin_hash": "$2a$10$whatever",
		"server_port":       "9000",
	}); err != nil {
		t.Fatalf("seed SaveInternal: %v", err)
	}
	if !Settings.IsSetupDone() {
		t.Fatal("precondition failed: setup should look done before ResetAll")
	}

	if err := Settings.ResetAll(); err != nil {
		t.Fatalf("ResetAll: %v", err)
	}

	if Settings.IsSetupDone() {
		t.Error("expected IsSetupDone() to be false immediately after ResetAll, no restart needed")
	}
	if got := Settings.GetString("server_port", ""); got != "" {
		t.Errorf("expected server_port to be cleared, got %q", got)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath): %v", err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}
	if len(onDisk) != 0 {
		t.Errorf("expected an empty config.json on disk after ResetAll, got %v", onDisk)
	}
}
