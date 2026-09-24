package api

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/managers"

	_ "modernc.org/sqlite"
)

const testFactoryResetSchema = `
CREATE TABLE pileus_devices (
	device_id    TEXT PRIMARY KEY,
	created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
	last_seen_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE pileus_profiles (
	profile_id  TEXT PRIMARY KEY,
	device_id   TEXT NOT NULL,
	name        TEXT NOT NULL,
	avatar_url  TEXT NOT NULL DEFAULT '',
	preferences TEXT NOT NULL DEFAULT '{}',
	created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE watch_history (
	client_id     TEXT,
	provider_id   TEXT,
	playable_id   TEXT,
	title         TEXT,
	progress_time REAL,
	total_time    REAL,
	is_completed  BOOLEAN,
	last_updated  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (client_id, provider_id, playable_id)
);
CREATE TABLE plugin_secrets (
	plugin_id  TEXT NOT NULL,
	profile_id TEXT NOT NULL,
	key        TEXT NOT NULL,
	value      TEXT NOT NULL,
	PRIMARY KEY (plugin_id, profile_id, key)
);`

// setupFactoryResetTestDB seeds one row in each of the three tables
// factoryResetHandler wipes, so a test can assert they're all actually gone
// afterward — not just that the call returned 200.
func setupFactoryResetTestDB(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(testFactoryResetSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pileus_devices(device_id) VALUES('dev1')`); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pileus_profiles(profile_id, device_id, name) VALUES('p1','dev1','Mario')`); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO watch_history(client_id, provider_id, playable_id) VALUES('p1','x','y')`); err != nil {
		t.Fatalf("seed watch_history: %v", err)
	}
	prev := managers.DB
	managers.DB = managers.NewDBManager(db)
	t.Cleanup(func() {
		db.Close()
		managers.DB = prev
	})
}

// withTestBasePath points core.BasePath (and so core.AppPath, e.g. the
// plugins/ dir clearDirContents/clearPluginSubdirs touch) at a throwaway
// directory for the test's duration.
func withTestBasePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := core.BasePath
	core.BasePath = dir
	t.Cleanup(func() { core.BasePath = prev })
	return dir
}

func TestFactoryReset_WrongPasswordRejectedNothingWiped(t *testing.T) {
	setupFactoryResetTestDB(t)
	withTestBasePath(t)
	SetAdminSessionKey([]byte("test-master-secret"))
	t.Cleanup(func() { SetAdminSessionKey(nil) })
	seedAdminPassword(t, "correct-horse-battery")

	req := httptest.NewRequest(http.MethodPost, "/admin/factory-reset", bytes.NewReader([]byte(`{"password":"wrong"}`)))
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: createAdminSession()})
	rec := httptest.NewRecorder()
	adminAuthMiddleware(factoryResetHandler)(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d: %s", rec.Code, rec.Body.String())
	}
	var count int
	if err := managers.DB.QueryRow(`SELECT COUNT(*) FROM pileus_profiles`).Scan(&count); err != nil {
		t.Fatalf("count profiles: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the seeded profile to survive a rejected reset, got %d rows", count)
	}
}

func TestFactoryReset_WipesEverythingAndClearsSetup(t *testing.T) {
	setupFactoryResetTestDB(t)
	base := withTestBasePath(t)
	SetAdminSessionKey([]byte("test-master-secret"))
	t.Cleanup(func() { SetAdminSessionKey(nil) })
	seedAdminPassword(t, "correct-horse-battery")

	// A fake installed plugin — a real reset must remove its directory, not
	// just its regenerated cache files (that's the scoped wipePluginDataHandler's
	// job, this is the harder "as if freshly installed" reset).
	pluginDir := filepath.Join(base, "plugins", "demo")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "init.lua"), []byte("-- demo"), 0644); err != nil {
		t.Fatalf("write plugin file: %v", err)
	}

	// Regression check: a factory reset once deleted this too, and a commit
	// of the now-empty plugins/ broke the next Docker build (`COPY plugins/`
	// has nothing to copy — git doesn't track empty directories). This file
	// must survive: it's a git/build placeholder, not plugin content.
	gitkeepPath := filepath.Join(base, "plugins", ".gitkeep")
	if err := os.WriteFile(gitkeepPath, nil, 0644); err != nil {
		t.Fatalf("write .gitkeep: %v", err)
	}

	if !managers.Settings.IsSetupDone() {
		t.Fatal("precondition failed: setup should look done before the reset")
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/factory-reset", bytes.NewReader([]byte(`{"password":"correct-horse-battery"}`)))
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: createAdminSession()})
	rec := httptest.NewRecorder()
	adminAuthMiddleware(factoryResetHandler)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	for _, table := range []string{"pileus_profiles", "pileus_devices", "watch_history"} {
		var count int
		if err := managers.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("expected %s to be empty after reset, got %d rows", table, count)
		}
	}

	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Errorf("expected plugin directory to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(gitkeepPath); err != nil {
		t.Errorf("expected plugins/.gitkeep to survive the reset, stat err=%v", err)
	}

	if managers.Settings.IsSetupDone() {
		t.Error("expected IsSetupDone() to be false immediately after the reset, no restart needed")
	}

	setCookie := rec.Result().Cookies()
	found := false
	for _, c := range setCookie {
		if c.Name == adminSessionCookie {
			found = true
			if c.MaxAge >= 0 {
				t.Errorf("expected the session cookie to be cleared (MaxAge<0), got %d", c.MaxAge)
			}
		}
	}
	if !found {
		t.Error("expected the response to clear the session cookie")
	}
}
