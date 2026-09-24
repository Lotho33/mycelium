package api

import (
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mycelium/internal/managers"
)

// withSetupNotDone clears master_admin_hash for the duration of the test, so
// saveSetup's IsSetupDone() guard doesn't short-circuit with 403 before
// reaching the password-confirmation check under test, and restores whatever
// hash was there before on cleanup.
func withSetupNotDone(t *testing.T) {
	t.Helper()
	old := managers.Settings.MasterAdminHash()
	if err := managers.Settings.SaveInternal(map[string]any{"master_admin_hash": ""}); err != nil {
		t.Fatalf("seed SaveInternal: %v", err)
	}
	t.Cleanup(func() {
		_ = managers.Settings.SaveInternal(map[string]any{"master_admin_hash": old})
	})
}

func postSetupSave(t *testing.T, password, confirm string) *httptest.ResponseRecorder {
	t.Helper()
	return postSetupSaveFrom(t, "192.168.1.20:5555", password, confirm)
}

func postSetupSaveFrom(t *testing.T, remoteAddr, password, confirm string) *httptest.ResponseRecorder {
	t.Helper()
	var b strings.Builder
	mw := multipart.NewWriter(&b)
	if err := mw.WriteField("master_admin_password", password); err != nil {
		t.Fatalf("WriteField password: %v", err)
	}
	if err := mw.WriteField("master_admin_password_confirm", confirm); err != nil {
		t.Fatalf("WriteField confirm: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/setup/save", strings.NewReader(b.String()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	saveSetup(rec, req)
	return rec
}

func TestSaveSetup_MismatchedConfirmationRejected(t *testing.T) {
	withSetupNotDone(t)

	rec := postSetupSave(t, "correct-horse-battery", "typo-horse-battery")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if managers.Settings.IsSetupDone() {
		t.Fatal("setup must not be marked done when the confirmation didn't match")
	}
}

func TestSaveSetup_MatchingConfirmationSucceeds(t *testing.T) {
	withSetupNotDone(t)

	rec := postSetupSave(t, "correct-horse-battery", "correct-horse-battery")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	if !managers.Settings.IsSetupDone() {
		t.Fatal("setup should be marked done after a matching password+confirmation")
	}
}

// Before an admin password exists (first boot, or after a factory reset)
// /setup/save is unauthenticated: it must refuse non-local clients so an
// internet-exposed node can't be claimed by whoever arrives first.
func TestSaveSetup_RejectsNonLocalClient(t *testing.T) {
	withSetupNotDone(t)

	rec := postSetupSaveFrom(t, "203.0.113.9:4444", "correct-horse-battery", "correct-horse-battery")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a public client, got %d: %s", rec.Code, rec.Body.String())
	}
	if managers.Settings.IsSetupDone() {
		t.Fatal("a public client must not be able to complete setup")
	}
}

func TestSetupAllowedFrom(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "192.168.1.5": true, "10.1.2.3": true,
		"172.16.0.1": true, "100.100.1.1": true, "fd00::1": true,
		"203.0.113.9": false, "8.8.8.8": false, "2001:db8::1": false, "garbage": false,
	} {
		if got := setupAllowedFrom(ip); got != want {
			t.Errorf("setupAllowedFrom(%s) = %v, want %v", ip, got, want)
		}
	}
}
