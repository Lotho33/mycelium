package pileus

import (
	"database/sql"
	"testing"

	"mycelium/internal/managers"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	_ "modernc.org/sqlite"
)

// testDeviceSchema is a minimal stand-in for the pileus_devices table InitDB
// creates in production (see internal/managers/database.go) — just the
// columns AuthorizeDevice / upsertDevice / RevokeDevice / deviceRevoked touch.
const testDeviceSchema = `
CREATE TABLE pileus_devices (
	device_id    TEXT PRIMARY KEY,
	created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
	last_seen_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
	label        TEXT NOT NULL DEFAULT '',
	revoked_at   TIMESTAMP
);`

// setupAuthTestDB points managers.DB at a throwaway in-memory sqlite database
// for the duration of the test and restores the previous global afterwards.
// SetMaxOpenConns(1) avoids the classic sqlite gotcha where a fresh
// ":memory:" connection from the pool is a brand new, empty database.
func setupAuthTestDB(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(testDeviceSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	prev := managers.DB
	managers.DB = managers.NewDBManager(db)
	t.Cleanup(func() {
		db.Close()
		managers.DB = prev
	})
}

// A never-before-seen device_id pairs normally: a row is inserted, not revoked.
func TestAuthorizeDeviceNewDeviceSucceeds(t *testing.T) {
	setupAuthTestDB(t)
	h := testAuthHandler()

	code, _ := GeneratePairingCode()
	resp, err := h.AuthorizeDevice(t.Context(), &gen.AuthorizeDeviceRequest{
		DeviceId: "device-new",
		PinHash:  code,
	})
	if err != nil {
		t.Fatalf("AuthorizeDevice: unexpected error: %v", err)
	}
	if resp.DeviceJwt == "" {
		t.Fatalf("AuthorizeDevice: empty JWT in response")
	}

	exists, revoked := deviceRevoked("device-new")
	if !exists {
		t.Fatalf("device row not inserted")
	}
	if revoked {
		t.Fatalf("newly paired device reported revoked")
	}
}

// A device_id that exists and is revoked must not be silently reactivated by
// a fresh, valid pairing code — the admin's revoke must stand until an
// explicit UnrevokeDevice.
func TestAuthorizeDeviceRevokedDeviceRejected(t *testing.T) {
	setupAuthTestDB(t)
	h := testAuthHandler()

	// Pair the device once so it exists, then have the admin revoke it.
	code1, _ := GeneratePairingCode()
	if _, err := h.AuthorizeDevice(t.Context(), &gen.AuthorizeDeviceRequest{
		DeviceId: "device-revoked",
		PinHash:  code1,
	}); err != nil {
		t.Fatalf("initial pairing failed: %v", err)
	}
	if err := RevokeDevice("device-revoked"); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if exists, revoked := deviceRevoked("device-revoked"); !exists || !revoked {
		t.Fatalf("device not revoked after RevokeDevice: exists=%v revoked=%v", exists, revoked)
	}

	// A malicious/compromised client races a fresh pairing code against this
	// already-revoked device_id.
	code2, _ := GeneratePairingCode()
	_, err := h.AuthorizeDevice(t.Context(), &gen.AuthorizeDeviceRequest{
		DeviceId: "device-revoked",
		PinHash:  code2,
	})
	if err == nil {
		t.Fatalf("AuthorizeDevice: revoked device was silently reactivated")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("AuthorizeDevice: got code %v, want PermissionDenied", status.Code(err))
	}

	// The revoke must still hold in the DB — not undone by the rejected attempt.
	if exists, revoked := deviceRevoked("device-revoked"); !exists || !revoked {
		t.Fatalf("device revocation was undone by rejected AuthorizeDevice: exists=%v revoked=%v", exists, revoked)
	}
}

// A device_id that exists and is NOT revoked keeps working — unchanged
// behavior, no regression from the fix above.
func TestAuthorizeDeviceActiveDeviceReauthorized(t *testing.T) {
	setupAuthTestDB(t)
	h := testAuthHandler()

	code1, _ := GeneratePairingCode()
	if _, err := h.AuthorizeDevice(t.Context(), &gen.AuthorizeDeviceRequest{
		DeviceId: "device-active",
		PinHash:  code1,
	}); err != nil {
		t.Fatalf("initial pairing failed: %v", err)
	}

	code2, _ := GeneratePairingCode()
	resp, err := h.AuthorizeDevice(t.Context(), &gen.AuthorizeDeviceRequest{
		DeviceId: "device-active",
		PinHash:  code2,
	})
	if err != nil {
		t.Fatalf("AuthorizeDevice: unexpected error re-authorizing active device: %v", err)
	}
	if resp.DeviceJwt == "" {
		t.Fatalf("AuthorizeDevice: empty JWT in response")
	}
	if exists, revoked := deviceRevoked("device-active"); !exists || revoked {
		t.Fatalf("active device unexpectedly revoked: exists=%v revoked=%v", exists, revoked)
	}
}
