package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// seedAdminPassword hashes and stores currentPassword as the admin hash,
// restoring whatever hash was there before once the test ends.
func seedAdminPassword(t *testing.T, currentPassword string) {
	t.Helper()
	old := managers.Settings.MasterAdminHash()
	hash, err := core.GetPasswordHash(currentPassword)
	if err != nil {
		t.Fatalf("GetPasswordHash: %v", err)
	}
	if err := managers.Settings.SaveInternal(map[string]any{"master_admin_hash": hash}); err != nil {
		t.Fatalf("seed SaveInternal: %v", err)
	}
	t.Cleanup(func() {
		_ = managers.Settings.SaveInternal(map[string]any{"master_admin_hash": old})
	})
}

func callChangeAdminPassword(t *testing.T, current, next string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"current_password": current, "new_password": next})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/password/change", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	changeAdminPasswordHandler(rec, req)
	return rec
}

// (b) wrong current password rejected — the pattern this fix relies on
// (requireAdminPassword in admin_wipe.go) applied to the password-change path.
func TestChangeAdminPassword_WrongCurrentRejected(t *testing.T) {
	seedAdminPassword(t, "correct-horse-battery")

	rec := callChangeAdminPassword(t, "totally-wrong", "brand-new-password")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	if err := core.VerifyAdmin("correct-horse-battery", managers.Settings.MasterAdminHash()); err != nil {
		t.Fatalf("hash must be unchanged after a rejected attempt: %v", err)
	}
}

// (c) correct current password updates the hash — verifiable afterwards with
// VerifyAdmin/bcrypt against the new password, and the old one stops working.
func TestChangeAdminPassword_SuccessUpdatesHash(t *testing.T) {
	seedAdminPassword(t, "correct-horse-battery")

	rec := callChangeAdminPassword(t, "correct-horse-battery", "brand-new-password")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", rec.Code, rec.Body.String())
	}

	newHash := managers.Settings.MasterAdminHash()
	if err := core.VerifyAdmin("brand-new-password", newHash); err != nil {
		t.Fatalf("new password does not verify against the stored hash: %v", err)
	}
	if err := core.VerifyAdmin("correct-horse-battery", newHash); err == nil {
		t.Fatal("old password still verifies after a successful change")
	}
}

// (d) a new password shorter than the setup minimum (setup.go: 8 chars) is
// rejected, and the hash is left untouched.
func TestChangeAdminPassword_ShortNewPasswordRejected(t *testing.T) {
	seedAdminPassword(t, "correct-horse-battery")

	rec := callChangeAdminPassword(t, "correct-horse-battery", "short")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	if err := core.VerifyAdmin("correct-horse-battery", managers.Settings.MasterAdminHash()); err != nil {
		t.Fatalf("hash must be unchanged after a rejected short password: %v", err)
	}
}

// (a, endpoint-level) the generic settings-save path must not accept
// master_admin_hash either — belt and suspenders alongside the
// managers.isAllowedKey unit tests in internal/managers/settings_test.go.
func TestSaveSettings_RejectsMasterAdminHash(t *testing.T) {
	seedAdminPassword(t, "correct-horse-battery")
	before := managers.Settings.MasterAdminHash()

	body, _ := json.Marshal(map[string]string{"master_admin_hash": "$2a$10$attacker-chosen-hash..............."})
	req := httptest.NewRequest(http.MethodPost, "/admin/settings/save", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	saveSettings(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("saveSettings must reject master_admin_hash, got 200: %s", rec.Body.String())
	}
	if managers.Settings.MasterAdminHash() != before {
		t.Fatal("master_admin_hash must not have changed via /admin/settings/save")
	}
}
