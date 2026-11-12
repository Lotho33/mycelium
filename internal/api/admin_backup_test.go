package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mycelium/internal/backup"
	"mycelium/internal/core"
	"mycelium/internal/managers"
)

func postBackupExport(t *testing.T, password, passphrase string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password, "passphrase": passphrase})
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/export", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	exportBackupHandler(rec, req)
	return rec
}

func postBackupInspect(t *testing.T, file []byte, passphrase string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("passphrase", passphrase)
	fw, _ := mw.CreateFormFile("file", "x"+backup.Ext)
	fw.Write(file)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/admin/backup/inspect", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	inspectBackupHandler(rec, req)
	return rec
}

func TestBackupExportInspectHandlers(t *testing.T) {
	oldBase := core.BasePath
	core.BasePath = t.TempDir()
	t.Cleanup(func() {
		core.BasePath = oldBase
		managers.Settings.Load()
	})
	os.MkdirAll(filepath.Join(core.BasePath, "data"), 0o700)
	managers.Settings.Load()
	seedAdminPassword(t, "correct-horse-battery")
	if err := managers.Settings.SaveInternal(map[string]any{"server_host": "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}

	if rec := postBackupExport(t, "wrong", ""); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong password: status %d", rec.Code)
	}
	rec := postBackupExport(t, "correct-horse-battery", "a long passphrase")
	if rec.Code != http.StatusOK {
		t.Fatalf("export: status %d: %s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, backup.Ext+`"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	file := rec.Body.Bytes()
	if !bytes.HasPrefix(file, []byte("MYCBK\x01")) {
		t.Fatal("export with a passphrase is not encrypted")
	}
	if bytes.Contains(file, []byte("10.0.0.5")) {
		t.Fatal("encrypted export leaks a setting in clear")
	}

	if rec := postBackupInspect(t, file, ""); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("inspect without passphrase: status %d", rec.Code)
	}
	rec = postBackupInspect(t, file, "a long passphrase")
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect: status %d: %s", rec.Code, rec.Body.String())
	}
	var sum backup.Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sum.Sections {
		for _, it := range s.Items {
			if s.Key == backup.SecSettings && it.ID == "server_host" && it.Status == backup.StatusSame {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("summary lacks server_host as unchanged: %+v", sum.Sections)
	}
	if st := backup.CurrentStatus(); st.Staged == nil {
		t.Error("status: nothing staged after inspect")
	}
	if err := backup.Discard(); err != nil {
		t.Fatal(err)
	}
}
