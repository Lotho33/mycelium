package pileus

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/managers"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const testPinSchema = testDeviceSchema + `
CREATE TABLE pileus_profiles (
	profile_id      TEXT PRIMARY KEY,
	device_id       TEXT NOT NULL,
	name            TEXT NOT NULL,
	avatar_url      TEXT NOT NULL DEFAULT '',
	preferences     TEXT NOT NULL DEFAULT '{}',
	access_pin_hash TEXT,
	created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE pileus_profile_trust (
	device_id  TEXT NOT NULL,
	profile_id TEXT NOT NULL,
	created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (device_id, profile_id)
);
CREATE TABLE plugin_secrets (
	plugin_id TEXT NOT NULL, profile_id TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL,
	PRIMARY KEY (plugin_id, profile_id, key)
);`

// setupPinTestDB: devices tv/phone, profiles "fam" (no PIN) and "me".
func setupPinTestDB(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(testPinSchema); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO pileus_devices(device_id,label) VALUES('tv','TV salotto'),('phone','Telefono')`,
		`INSERT INTO pileus_profiles(profile_id,device_id,name) VALUES('fam','tv','Famiglia'),('me','tv','Personale')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	prev := managers.DB
	managers.DB = managers.NewDBManager(db)
	clearPinCache()
	profileSessions.drop("", "")
	pinAttempts.mu.Lock()
	pinAttempts.m = map[string]*pinMissState{}
	pinAttempts.mu.Unlock()
	t.Cleanup(func() {
		db.Close()
		managers.DB = prev
		clearPinCache()
		profileSessions.drop("", "")
	})
}

func devCtx(device string, md ...string) context.Context {
	ctx := context.WithValue(context.Background(), ctxKeyDeviceID{}, device)
	for i := 0; i+1 < len(md); i += 2 {
		if md[i] == "x-profile-session" {
			ctx = context.WithValue(ctx, ctxKeyProfileSession{}, md[i+1])
		}
	}
	return ctx
}

// profCtx is devCtx with the active profile set, as authenticate() does for
// x-profile-id.
func profCtx(device, profile string) context.Context {
	return context.WithValue(devCtx(device), ctxKeyProfileID{}, profile)
}

func wantCode(t *testing.T, err error, c codes.Code) {
	t.Helper()
	if s, _ := status.FromError(err); s.Code() != c {
		t.Fatalf("err = %v, want %v", err, c)
	}
}

// callThrough runs a MediaPipeline call through the real interceptor.
func callThrough(t *testing.T, h *AuthHandler, device, profile, session string) error {
	t.Helper()
	tok, err := h.mintJWT(device, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	md := metadata.Pairs("authorization", "Bearer "+tok, "x-profile-id", profile)
	if session != "" {
		md.Append("x-profile-session", session)
	}
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err = authInterceptor(h)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/mycelium.MediaPipeline/GetContinueWatching"}, okHandler)
	return err
}

func TestProfilePin_FamilyTrustedAndOneOffSession(t *testing.T) {
	setupPinTestDB(t)
	h := testAuthHandler()
	orig := deviceActiveLookup
	deviceActiveLookup = func(string) bool { return true }
	t.Cleanup(func() { deviceActiveLookup = orig })
	deviceActiveCache.Delete("tv")
	deviceActiveCache.Delete("phone")

	// Both profiles get a PIN (set from the TV: no PIN yet, no current_pin).
	for _, pid := range []string{"fam", "me"} {
		if _, err := h.SetProfilePin(profCtx("tv", pid), &gen.SetProfilePinRequest{ProfileId: pid, NewPin: "1234"}); err != nil {
			t.Fatal(err)
		}
	}
	wantCode(t, callThrough(t, h, "tv", "fam", ""), codes.PermissionDenied)

	// The family profile is unlocked once with "remember": trusted for good.
	r, err := h.UnlockProfile(devCtx("tv"), &gen.UnlockProfileRequest{ProfileId: "fam", Pin: "1234", RememberDevice: true})
	if err != nil || !r.DeviceTrusted || r.SessionToken != "" {
		t.Fatalf("remember unlock: %+v %v", r, err)
	}
	if err := callThrough(t, h, "tv", "fam", ""); err != nil {
		t.Fatalf("trusted family profile refused: %v", err)
	}

	// "Just this time" for my own profile on the family TV.
	r, err = h.UnlockProfile(devCtx("tv"), &gen.UnlockProfileRequest{ProfileId: "me", Pin: "1234"})
	if err != nil || r.SessionToken == "" || r.DeviceTrusted {
		t.Fatalf("one-off unlock: %+v %v", r, err)
	}
	if err := callThrough(t, h, "tv", "me", r.SessionToken); err != nil {
		t.Fatalf("session token refused: %v", err)
	}
	// Without the token (app restarted) it's locked again...
	wantCode(t, callThrough(t, h, "tv", "me", ""), codes.PermissionDenied)
	// ...the token is useless on another device or another profile...
	wantCode(t, callThrough(t, h, "phone", "me", r.SessionToken), codes.PermissionDenied)
	// ...and LockProfile (leaving the profile) kills it.
	if _, err := h.LockProfile(devCtx("tv"), &gen.LockProfileRequest{ProfileId: "me"}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, callThrough(t, h, "tv", "me", r.SessionToken), codes.PermissionDenied)
	// The family trust survives all of this.
	if err := callThrough(t, h, "tv", "fam", ""); err != nil {
		t.Fatalf("family trust lost: %v", err)
	}

	// ListProfiles reports the flags per device.
	lr, err := h.ListProfiles(devCtx("tv"), &gen.ListProfilesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range lr.Profiles {
		switch p.ProfileId {
		case "fam":
			if !p.PinProtected || !p.DeviceTrusted || !p.Unlocked {
				t.Errorf("fam flags %+v", p)
			}
		case "me":
			if !p.PinProtected || p.DeviceTrusted || p.Unlocked {
				t.Errorf("me flags %+v", p)
			}
		}
	}
}

func TestProfilePin_WrongPinLockout(t *testing.T) {
	setupPinTestDB(t)
	h := testAuthHandler()
	if err := replaceProfilePin("me", "4321"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pinFreeMisses; i++ {
		_, err := h.UnlockProfile(devCtx("phone"), &gen.UnlockProfileRequest{ProfileId: "me", Pin: "0000"})
		wantCode(t, err, codes.PermissionDenied)
	}
	// Locked out now — even the right PIN waits.
	_, err := h.UnlockProfile(devCtx("phone"), &gen.UnlockProfileRequest{ProfileId: "me", Pin: "4321"})
	wantCode(t, err, codes.ResourceExhausted)
	// Other devices aren't affected by this device's lockout.
	if _, err := h.UnlockProfile(devCtx("tv"), &gen.UnlockProfileRequest{ProfileId: "me", Pin: "4321"}); err != nil {
		t.Fatalf("tv locked out by phone's misses: %v", err)
	}
	// Time passes: the phone may try again.
	pinAttempts.mu.Lock()
	pinAttempts.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	pinAttempts.mu.Unlock()
	t.Cleanup(func() { pinAttempts.mu.Lock(); pinAttempts.now = time.Now; pinAttempts.mu.Unlock() })
	if _, err := h.UnlockProfile(devCtx("phone"), &gen.UnlockProfileRequest{ProfileId: "me", Pin: "4321"}); err != nil {
		t.Fatalf("after lockout: %v", err)
	}
}

// A burst of concurrent wrong guesses must not slip past the limiter: only
// the free misses may reach bcrypt, the rest are refused as locked out.
func TestProfilePin_ConcurrentGuessesStillLimited(t *testing.T) {
	setupPinTestDB(t)
	h := testAuthHandler()
	if err := replaceProfilePin("me", "4321"); err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	var denied, locked atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := h.UnlockProfile(devCtx("phone"), &gen.UnlockProfileRequest{ProfileId: "me", Pin: fmt.Sprintf("%04d", i)})
			switch s, _ := status.FromError(err); s.Code() {
			case codes.PermissionDenied:
				denied.Add(1)
			case codes.ResourceExhausted:
				locked.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if denied.Load() != pinFreeMisses || locked.Load() != n-pinFreeMisses {
		t.Fatalf("wrong PIN checked %d times, locked out %d (want %d / %d)", denied.Load(), locked.Load(), pinFreeMisses, n-pinFreeMisses)
	}
}

func TestProfilePin_ChangeRevokesTrustAndNeedsCurrentPin(t *testing.T) {
	setupPinTestDB(t)
	h := testAuthHandler()
	if err := replaceProfilePin("me", "1111"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.UnlockProfile(devCtx("tv"), &gen.UnlockProfileRequest{ProfileId: "me", Pin: "1111", RememberDevice: true}); err != nil {
		t.Fatal(err)
	}
	_, err := h.SetProfilePin(profCtx("phone", "me"), &gen.SetProfilePinRequest{ProfileId: "me", CurrentPin: "9999", NewPin: "2222"})
	wantCode(t, err, codes.PermissionDenied)
	_, err = h.SetProfilePin(profCtx("phone", "me"), &gen.SetProfilePinRequest{ProfileId: "me", CurrentPin: "1111", NewPin: "12"})
	wantCode(t, err, codes.InvalidArgument)
	if _, err := h.SetProfilePin(profCtx("phone", "me"), &gen.SetProfilePinRequest{ProfileId: "me", CurrentPin: "1111", NewPin: "2222"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := profileAccessible("tv", "me", ""); ok {
		t.Fatal("trust survived a PIN change")
	}
	// Protected handlers acting on a profile named in the request.
	_, err = h.DeleteProfile(devCtx("phone"), &gen.DeleteProfileRequest{ProfileId: "me"})
	wantCode(t, err, codes.PermissionDenied)
	// Removing the PIN opens the profile to everyone.
	if _, err := h.SetProfilePin(profCtx("phone", "me"), &gen.SetProfilePinRequest{ProfileId: "me", CurrentPin: "2222"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := profileAccessible("phone", "me", ""); !ok {
		t.Fatal("profile still locked after removing the PIN")
	}
}

func TestProfileSession_IdleExpiry(t *testing.T) {
	s := &profileSessionStore{m: map[string]*profileSession{}, now: time.Now}
	base := time.Now()
	s.now = func() time.Time { return base }
	tok, _, _ := s.create("tv", "me")
	s.now = func() time.Time { return base.Add(profileSessionIdle - time.Minute) }
	if !s.valid(tok, "tv", "me") {
		t.Fatal("expired too early")
	}
	s.now = func() time.Time { return base.Add(2*profileSessionIdle - time.Minute) }
	if !s.valid(tok, "tv", "me") {
		t.Fatal("use must slide the idle expiry")
	}
	s.now = func() time.Time { return base.Add(profileSessionMax + time.Minute) }
	if s.valid(tok, "tv", "me") {
		t.Fatal("absolute cap not enforced")
	}
}

// Re-pairing an existing device id (anyone with a pairing code can claim
// one) must not inherit its PIN trusts; an id equal to a profile's is refused.
func TestAuthorizeDevice_RepairDropsTrustAndRejectsProfileID(t *testing.T) {
	setupPinTestDB(t)
	h := testAuthHandler()
	if err := replaceProfilePin("fam", "1234"); err != nil {
		t.Fatal(err)
	}
	if err := trustDevice("tv", "fam"); err != nil {
		t.Fatal(err)
	}
	code, _ := GeneratePairingCode()
	if _, err := h.AuthorizeDevice(context.Background(), &gen.AuthorizeDeviceRequest{DeviceId: "tv", PinHash: code}); err != nil {
		t.Fatalf("re-pair: %v", err)
	}
	if ok, _ := profileAccessible("tv", "fam", ""); ok {
		t.Fatal("re-paired device id kept its PIN trust")
	}

	code, _ = GeneratePairingCode()
	_, err := h.AuthorizeDevice(context.Background(), &gen.AuthorizeDeviceRequest{DeviceId: "fam", PinHash: code})
	wantCode(t, err, codes.InvalidArgument)
	if !VerifyPairingCode(code, "x") {
		t.Fatal("a refused device_id must not spend the pairing code")
	}
}

// A PIN is set from inside the profile (or the dashboard), never from
// another profile or with no profile at all.
func TestSetProfilePin_OnlyFromTheProfileItself(t *testing.T) {
	setupPinTestDB(t)
	h := testAuthHandler()
	_, err := h.SetProfilePin(profCtx("phone", "fam"), &gen.SetProfilePinRequest{ProfileId: "me", NewPin: "1234"})
	wantCode(t, err, codes.PermissionDenied)
	_, err = h.SetProfilePin(devCtx("phone"), &gen.SetProfilePinRequest{ProfileId: "me", NewPin: "1234"})
	wantCode(t, err, codes.PermissionDenied)
	if p, _ := profileProtected("me"); p {
		t.Fatal("PIN set from outside the profile")
	}
	if _, err := h.SetProfilePin(profCtx("phone", "me"), &gen.SetProfilePinRequest{ProfileId: "me", NewPin: "1234"}); err != nil {
		t.Fatalf("own profile: %v", err)
	}
}
