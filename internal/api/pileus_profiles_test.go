package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mycelium/internal/managers"

	_ "modernc.org/sqlite"
)

const testProfileSchema = `
CREATE TABLE pileus_profiles (
	profile_id  TEXT PRIMARY KEY,
	device_id   TEXT NOT NULL,
	name        TEXT NOT NULL,
	avatar_url  TEXT NOT NULL DEFAULT '',
	preferences TEXT NOT NULL DEFAULT '{}',
	created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);`

func setupProfileAPITestDB(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(testProfileSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	prev := managers.DB
	managers.DB = managers.NewDBManager(db)
	t.Cleanup(func() {
		db.Close()
		managers.DB = prev
	})
}

// authedProfileRequest builds a request through the real adminAuthMiddleware
// chain, same as production routing (RegisterPileusProfileRoutes wraps every
// handler in it), with a freshly minted, valid session cookie.
func authedProfileRequest(t *testing.T, method, path string, body []byte, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	SetAdminSessionKey([]byte("test-master-secret"))
	t.Cleanup(func() { SetAdminSessionKey(nil) })

	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: createAdminSession()})
	rec := httptest.NewRecorder()
	adminAuthMiddleware(handler)(rec, req)
	return rec
}

func TestCreatePileusProfile_MissingNameRejected(t *testing.T) {
	setupProfileAPITestDB(t)
	rec := authedProfileRequest(t, http.MethodPost, "/admin/pileus/profiles",
		[]byte(`{"avatar_url":"x.png"}`), createPileusProfile)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateThenListPileusProfile(t *testing.T) {
	setupProfileAPITestDB(t)

	rec := authedProfileRequest(t, http.MethodPost, "/admin/pileus/profiles",
		[]byte(`{"name":"Mario","avatar_url":"a.png"}`), createPileusProfile)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ProfileID string `json:"profile_id"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ProfileID == "" || created.Name != "Mario" {
		t.Fatalf("unexpected create response: %+v", created)
	}

	rec = authedProfileRequest(t, http.MethodGet, "/admin/pileus/profiles", nil, listPileusProfiles)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Profiles []struct {
			ProfileID string `json:"profile_id"`
			Name      string `json:"name"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed.Profiles) != 1 || listed.Profiles[0].ProfileID != created.ProfileID {
		t.Fatalf("unexpected list response: %+v", listed)
	}
}

func TestUpdatePileusProfile_MissingIDRejected(t *testing.T) {
	setupProfileAPITestDB(t)
	// PathValue("id") is only populated by the real mux routing a {id}
	// pattern, not by calling the handler directly — this exercises the
	// explicit empty-id guard the handler falls back to.
	rec := authedProfileRequest(t, http.MethodPost, "/admin/pileus/profiles/",
		[]byte(`{"name":"x"}`), updatePileusProfile)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeletePileusProfile_RemovesIt(t *testing.T) {
	setupProfileAPITestDB(t)

	rec := authedProfileRequest(t, http.MethodPost, "/admin/pileus/profiles",
		[]byte(`{"name":"Delete me"}`), createPileusProfile)
	var created struct {
		ProfileID string `json:"profile_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	mux := http.NewServeMux()
	RegisterPileusProfileRoutes(mux)
	SetAdminSessionKey([]byte("test-master-secret"))
	t.Cleanup(func() { SetAdminSessionKey(nil) })

	req := httptest.NewRequest(http.MethodDelete, "/admin/pileus/profiles/"+created.ProfileID, nil)
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: createAdminSession()})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = authedProfileRequest(t, http.MethodGet, "/admin/pileus/profiles", nil, listPileusProfiles)
	var listed struct {
		Profiles []any `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed.Profiles) != 0 {
		t.Fatalf("expected 0 profiles after delete, got %d", len(listed.Profiles))
	}
}

func TestPileusProfileRoutes_RejectUnauthenticated(t *testing.T) {
	setupProfileAPITestDB(t)
	SetAdminSessionKey([]byte("test-master-secret"))
	t.Cleanup(func() { SetAdminSessionKey(nil) })

	mux := http.NewServeMux()
	RegisterPileusProfileRoutes(mux)

	// adminAuthMiddleware's default (no matching Accept/X-Requested-With
	// header) is a 303 redirect to /admin/login, not a 401 — and this is
	// what the dashboard's own apiFetch (admin.js) actually gets too: it
	// sends a plain fetch() with no Accept override, so an expired/missing
	// session redirects rather than 401s. Noted here, not fixed — unrelated
	// to this feature, same behavior on every other /admin/* route already.
	req := httptest.NewRequest(http.MethodGet, "/admin/pileus/profiles", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect with no session cookie, got %d", rec.Code)
	}

	// The JSON-401 branch does exist and does work for a client that
	// actually asks for it (e.g. an XHR-style caller, or apiFetch if it's
	// ever changed to send this) — covered so it doesn't silently rot.
	req = httptest.NewRequest(http.MethodGet, "/admin/pileus/profiles", nil)
	req.Header.Set("Accept", "application/json")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with Accept: application/json and no session cookie, got %d", rec.Code)
	}
}
