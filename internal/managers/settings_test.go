package managers

import (
	"encoding/json"
	"os"
	"testing"
)

// The backdoor this closes: master_admin_hash used to be in the generic
// allowlist, writable verbatim (no bcrypt, no current-password check) via
// POST /admin/settings/save — a stolen session cookie was enough to plant a
// chosen admin hash. It must stay out of isAllowedKey/Save; only
// SaveInternal (setup.go one-shot + admin_password.go, after verifying the
// current password) may write it.
func TestIsAllowedKey_RejectsMasterAdminHash(t *testing.T) {
	if isAllowedKey("master_admin_hash") {
		t.Fatal("master_admin_hash must not be in the generic settings allowlist")
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
