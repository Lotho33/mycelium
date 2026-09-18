package pileus

import (
	"database/sql"
	"testing"

	"mycelium/internal/managers"

	_ "modernc.org/sqlite"
)

// testProfileSchema mirrors the columns ListProfilesAdmin/CreateProfileAdmin/
// UpdateProfileAdmin/DeleteProfileAdmin touch (see internal/managers/database.go
// for the production schema — device_id has no FK here since it's audit-only
// and the real DB never enforces it either, see DeleteDevice's comment).
const testProfileSchema = `
CREATE TABLE pileus_profiles (
	profile_id  TEXT PRIMARY KEY,
	device_id   TEXT NOT NULL,
	name        TEXT NOT NULL,
	avatar_url  TEXT NOT NULL DEFAULT '',
	preferences TEXT NOT NULL DEFAULT '{}',
	created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);`

func setupProfileTestDB(t *testing.T) {
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

func TestCreateProfileAdmin_ThenListedAndPersisted(t *testing.T) {
	setupProfileTestDB(t)

	p, err := CreateProfileAdmin("Mario", "https://example.com/a.png")
	if err != nil {
		t.Fatalf("CreateProfileAdmin: %v", err)
	}
	if p.ProfileID == "" {
		t.Fatal("expected a generated profile id")
	}

	list, err := ListProfilesAdmin()
	if err != nil {
		t.Fatalf("ListProfilesAdmin: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(list))
	}
	if list[0].Name != "Mario" || list[0].AvatarURL != "https://example.com/a.png" {
		t.Fatalf("unexpected profile: %+v", list[0])
	}
}

func TestListProfilesAdmin_ServerWideNotDeviceScoped(t *testing.T) {
	setupProfileTestDB(t)

	// A profile created "by a device" (as a real Pileus client would via
	// gRPC CreateProfile) must show up in the admin list exactly like a
	// dashboard-created one — profiles are server-wide, not per-device.
	if err := insertProfile("some-real-device-id", "p1", "Luigi", "", "{}"); err != nil {
		t.Fatalf("insertProfile: %v", err)
	}
	if _, err := CreateProfileAdmin("Peach", ""); err != nil {
		t.Fatalf("CreateProfileAdmin: %v", err)
	}

	list, err := ListProfilesAdmin()
	if err != nil {
		t.Fatalf("ListProfilesAdmin: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(list))
	}
}

func TestUpdateProfileAdmin_OverwritesFields(t *testing.T) {
	setupProfileTestDB(t)

	p, err := CreateProfileAdmin("Mario", "old.png")
	if err != nil {
		t.Fatalf("CreateProfileAdmin: %v", err)
	}

	if err := UpdateProfileAdmin(p.ProfileID, "Mario Bros", "new.png"); err != nil {
		t.Fatalf("UpdateProfileAdmin: %v", err)
	}

	list, err := ListProfilesAdmin()
	if err != nil {
		t.Fatalf("ListProfilesAdmin: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(list))
	}
	got := list[0]
	if got.Name != "Mario Bros" || got.AvatarURL != "new.png" {
		t.Fatalf("update didn't apply: %+v", got)
	}
}

func TestDeleteProfileAdmin_RemovesOnlyThatProfile(t *testing.T) {
	setupProfileTestDB(t)

	keep, err := CreateProfileAdmin("Keep me", "")
	if err != nil {
		t.Fatalf("CreateProfileAdmin: %v", err)
	}
	gone, err := CreateProfileAdmin("Delete me", "")
	if err != nil {
		t.Fatalf("CreateProfileAdmin: %v", err)
	}

	if err := DeleteProfileAdmin(gone.ProfileID); err != nil {
		t.Fatalf("DeleteProfileAdmin: %v", err)
	}

	list, err := ListProfilesAdmin()
	if err != nil {
		t.Fatalf("ListProfilesAdmin: %v", err)
	}
	if len(list) != 1 || list[0].ProfileID != keep.ProfileID {
		t.Fatalf("expected only %q to survive, got %+v", keep.ProfileID, list)
	}
}
