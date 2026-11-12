package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mycelium/internal/backup"
	"mycelium/internal/core"
	"mycelium/internal/managers"
)

func setupRestoreRequest(t *testing.T, h http.HandlerFunc, method, target string, body *bytes.Buffer, contentType, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.Host = "192.168.1.10:8000"
	req.RemoteAddr = remote
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func setupRestoreUpload(t *testing.T, file []byte, remote string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("passphrase", "")
	fw, _ := mw.CreateFormFile("file", "old"+backup.Ext)
	fw.Write(file)
	mw.Close()
	return setupRestoreRequest(t, setupRestoreInspect, http.MethodPost, "/setup/restore/inspect", &buf, mw.FormDataContentType(), remote)
}

func setupRestoreApplyJSON(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	return setupRestoreRequest(t, setupRestoreApply, http.MethodPost, "/setup/restore/apply", bytes.NewBuffer(b), "application/json", "192.168.1.20:40000")
}

func useFreshBase(t *testing.T) {
	t.Helper()
	core.BasePath = t.TempDir()
	os.MkdirAll(filepath.Join(core.BasePath, "data"), 0o700)
	managers.Settings.Load()
}

func TestSetupRestore(t *testing.T) {
	oldBase := core.BasePath
	t.Cleanup(func() {
		core.BasePath = oldBase
		managers.Settings.Load()
	})
	restarts := 0
	oldRestart := setupRestart
	setupRestart = func() { restarts++ }
	t.Cleanup(func() { setupRestart = oldRestart })

	// The old server: an admin password and a setting, exported.
	useFreshBase(t)
	oldHash, _ := core.GetPasswordHash("old-server-password")
	if err := managers.Settings.SaveInternal(map[string]any{"master_admin_hash": oldHash, "server_host": "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	if err := backup.Export(&file, ""); err != nil {
		t.Fatal(err)
	}

	// The new server, not set up yet.
	useFreshBase(t)
	if managers.Settings.IsSetupDone() {
		t.Fatal("fresh base is already set up")
	}

	if rec := setupRestoreUpload(t, file.Bytes(), "203.0.113.9:5000"); rec.Code != http.StatusForbidden {
		t.Fatalf("inspect from a public address: %d", rec.Code)
	}
	rec := setupRestoreUpload(t, file.Bytes(), "192.168.1.20:40000")
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect: %d %s", rec.Code, rec.Body.String())
	}

	// Without the backup's password, a new one is required.
	sel := map[string][]string{"settings": {"server_host"}}
	if rec := setupRestoreApplyJSON(t, map[string]any{"selection": sel}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no password: %d", rec.Code)
	}
	if managers.Settings.IsSetupDone() || backup.CurrentStatus().Pending {
		t.Fatal("a refused request changed something")
	}
	// Nothing selected: refused before the password is written.
	if rec := setupRestoreApplyJSON(t, map[string]any{"selection": map[string][]string{}, "password": "new-password-1", "password_confirm": "new-password-1"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty selection: %d", rec.Code)
	}
	if managers.Settings.IsSetupDone() {
		t.Fatal("empty selection closed the setup")
	}

	sel["identity"] = []string{backup.ItemAdminPassword}
	rec = setupRestoreApplyJSON(t, map[string]any{"selection": sel})
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	if managers.Settings.MasterAdminHash() != oldHash {
		t.Fatal("the backup's admin password wasn't written before the restart")
	}
	if !backup.CurrentStatus().Pending || restarts != 1 {
		t.Fatalf("restore not scheduled (pending=%v restarts=%d)", backup.CurrentStatus().Pending, restarts)
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Error("no session cookie for the browser that ran the setup")
	}

	// The setup is closed now.
	if rec := setupRestoreUpload(t, file.Bytes(), "192.168.1.20:40000"); rec.Code != http.StatusForbidden {
		t.Fatalf("inspect after setup: %d", rec.Code)
	}
	if rec := setupRestoreApplyJSON(t, map[string]any{"selection": sel}); rec.Code != http.StatusForbidden {
		t.Fatalf("apply after setup: %d", rec.Code)
	}

	// At the next boot the rest is applied.
	backup.ApplyPending()
	managers.Settings.Load()
	if got := managers.Settings.GetString("server_host", ""); got != "10.0.0.5" {
		t.Fatalf("server_host after boot = %q", got)
	}
}

func TestSetupRestoreNewPassword(t *testing.T) {
	oldBase := core.BasePath
	t.Cleanup(func() {
		core.BasePath = oldBase
		managers.Settings.Load()
	})
	oldRestart := setupRestart
	setupRestart = func() {}
	t.Cleanup(func() { setupRestart = oldRestart })

	useFreshBase(t)
	oldHash, _ := core.GetPasswordHash("old-server-password")
	managers.Settings.SaveInternal(map[string]any{"master_admin_hash": oldHash, "server_host": "10.0.0.5"})
	var file bytes.Buffer
	if err := backup.Export(&file, ""); err != nil {
		t.Fatal(err)
	}
	useFreshBase(t)
	if rec := setupRestoreUpload(t, file.Bytes(), "192.168.1.20:40000"); rec.Code != http.StatusOK {
		t.Fatalf("inspect: %d", rec.Code)
	}
	sel := map[string][]string{"settings": {"server_host"}}
	if rec := setupRestoreApplyJSON(t, map[string]any{"selection": sel, "password": "new-password-1", "password_confirm": "other"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched passwords: %d", rec.Code)
	}
	if rec := setupRestoreApplyJSON(t, map[string]any{"selection": sel, "password": "new-password-1", "password_confirm": "new-password-1"}); rec.Code != http.StatusOK {
		t.Fatalf("apply: %d", rec.Code)
	}
	if err := core.VerifyAdmin("new-password-1", managers.Settings.MasterAdminHash()); err != nil {
		t.Fatal("the new password wasn't set")
	}
	// The boot restore must not bring the old password back: it wasn't picked.
	backup.ApplyPending()
	managers.Settings.Load()
	if err := core.VerifyAdmin("new-password-1", managers.Settings.MasterAdminHash()); err != nil {
		t.Fatal("the old admin password came back at boot")
	}
}
