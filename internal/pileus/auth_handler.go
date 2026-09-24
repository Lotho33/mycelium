// Package pileus implements the gRPC server that Pileus TV clients connect to.
// AuthService handles device pairing (master PIN) and profile management.
package pileus

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"mycelium/internal/managers"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const jwtTTL = 30 * 24 * time.Hour // 30-day device token

// AuthHandler implements gen.AuthServiceServer.
type AuthHandler struct {
	gen.UnimplementedAuthServiceServer
	jwtSecret []byte
}

// NewAuthHandler returns a ready AuthHandler. jwtSecret must be stable across
// restarts (load from settings / env); if empty it panics. Callers should
// pass the output of DeriveJWTSecret, not the raw master secret — see there.
func NewAuthHandler(jwtSecret []byte) *AuthHandler {
	if len(jwtSecret) == 0 {
		panic("pileus: jwt secret must not be empty")
	}
	return &AuthHandler{jwtSecret: jwtSecret}
}

// DeriveJWTSecret derives the key used to sign/verify Pileus device JWTs from
// the shared Pileus master secret ("pileus_jwt_secret" in settings), via
// HMAC-SHA256 with a domain-separation constant. This mirrors
// core.SetProxySignKey and api.SetAdminSessionKey, which derive their own
// subkeys from the very same master with a different constant each — so the
// one master secret never ends up signing three different things with the
// same raw key material, and each use could in principle be rotated on its
// own without touching the other two. Call once at startup and pass the
// result to NewAuthHandler / Start; never pass the raw master secret there.
func DeriveJWTSecret(master []byte) []byte {
	if len(master) == 0 {
		return nil
	}
	m := hmac.New(sha256.New, master)
	m.Write([]byte("mycelium/jwt/v1"))
	return m.Sum(nil)
}

// ─────────────────────────────────────────────────────────────────────────────
// AuthorizeDevice
// ─────────────────────────────────────────────────────────────────────────────

func (h *AuthHandler) AuthorizeDevice(ctx context.Context, req *gen.AuthorizeDeviceRequest) (*gen.AuthorizeDeviceResponse, error) {
	if req.DeviceId == "" || req.PinHash == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id and pin_hash required")
	}

	// req.PinHash carries whatever string the Pileus UI's pairing field
	// collected — a misleading field name kept as-is for wire compatibility
	// (the Pileus client already POSTs a free-form string here; no proto or
	// client change needed to switch what's expected in it). It used to be
	// compared against the real admin password hash — see git history on
	// this line for why that was a problem (every new device, including a
	// TV reached only by a remote's on-screen keyboard, needed the same
	// secret that unlocks the whole admin panel, with no way to revoke one
	// device without rotating that password for everything). Now it's a
	// short-lived rotating code generated from the dashboard (see
	// pairing.go) — a separate secret space, single-use, expires on its own.
	if !VerifyPairingCode(req.PinHash, req.DeviceId) {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired pairing code")
	}

	// A valid pairing code alone must never resurrect a device the admin has
	// explicitly revoked — device_id is picked by the client, not tied to the
	// code, so anyone who knows a revoked device's id (ListDevices exposes ids
	// to any paired device) and can submit the code first wins the race
	// against a human typing it on a remote. Reactivation must go through the
	// explicit admin action (UnrevokeDevice / POST .../unrevoke) instead.
	if exists, revoked := deviceRevoked(req.DeviceId); exists && revoked {
		return nil, status.Error(codes.PermissionDenied, "device revoked; ask an admin to re-enable it from the dashboard")
	}

	// Upsert device record.
	if err := upsertDevice(req.DeviceId); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}

	expiresAt := time.Now().Add(jwtTTL)
	tokenStr, err := h.mintJWT(req.DeviceId, expiresAt)
	if err != nil {
		return nil, status.Error(codes.Internal, "jwt mint error")
	}

	log.Printf("[pileus/auth] device %s authorized", req.DeviceId)
	return &gen.AuthorizeDeviceResponse{
		DeviceJwt: tokenStr,
		ExpiresAt: expiresAt.Unix(),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Profile CRUD
// ─────────────────────────────────────────────────────────────────────────────

func (h *AuthHandler) CreateProfile(ctx context.Context, req *gen.CreateProfileRequest) (*gen.ProfileResponse, error) {
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name required")
	}

	profileID := newID()

	prefs, err := normalizePrefsJSON(req.PreferencesJson)
	if err != nil {
		return nil, err
	}

	if err := insertProfile(deviceID, profileID, req.Name, req.AvatarUrl, prefs); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}

	return &gen.ProfileResponse{
		ProfileId:       profileID,
		Name:            req.Name,
		AvatarUrl:       req.AvatarUrl,
		PreferencesJson: prefs,
	}, nil
}

func (h *AuthHandler) ListProfiles(ctx context.Context, _ *gen.ListProfilesRequest) (*gen.ListProfilesResponse, error) {
	if _, err := h.deviceFromCtx(ctx); err != nil {
		return nil, err
	}
	profiles, err := listProfiles()
	if err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	return &gen.ListProfilesResponse{Profiles: profiles}, nil
}

func (h *AuthHandler) DeleteProfile(ctx context.Context, req *gen.DeleteProfileRequest) (*gen.DeleteProfileResponse, error) {
	if _, err := h.deviceFromCtx(ctx); err != nil {
		return nil, err
	}
	if req.ProfileId == "" {
		return nil, status.Error(codes.InvalidArgument, "profile_id required")
	}
	if err := deleteProfile(req.ProfileId); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	return &gen.DeleteProfileResponse{Ok: true}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// UpdateProfile
// ─────────────────────────────────────────────────────────────────────────────

func (h *AuthHandler) UpdateProfile(ctx context.Context, req *gen.UpdateProfileRequest) (*gen.ProfileResponse, error) {
	if _, err := h.deviceFromCtx(ctx); err != nil {
		return nil, err
	}
	if req.ProfileId == "" {
		return nil, status.Error(codes.InvalidArgument, "profile_id required")
	}

	// Load current values.
	row, err := getProfileRow(req.ProfileId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "profile not found")
	}

	name := row.name
	if req.Name != "" {
		name = req.Name
	}
	avatarURL := row.avatarURL
	if req.AvatarUrl != "" {
		avatarURL = req.AvatarUrl
	}

	if err := updateProfile(req.ProfileId, name, avatarURL); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	// UpdateProfile never touches preferences — echo the stored blob.
	return &gen.ProfileResponse{
		ProfileId:       req.ProfileId,
		Name:            name,
		AvatarUrl:       avatarURL,
		PreferencesJson: row.preferences,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// SetProfilePreferences — replaces only the opaque per-profile preferences
// blob. Separate from UpdateProfile so the client doesn't round-trip
// name/avatar just to save a setting. Profiles are server-wide, so the blob
// then follows the person to every paired device.
// ─────────────────────────────────────────────────────────────────────────────

func (h *AuthHandler) SetProfilePreferences(ctx context.Context, req *gen.SetProfilePreferencesRequest) (*gen.ProfileResponse, error) {
	if _, err := h.deviceFromCtx(ctx); err != nil {
		return nil, err
	}
	if req.ProfileId == "" {
		return nil, status.Error(codes.InvalidArgument, "profile_id required")
	}

	prefs, err := normalizePrefsJSON(req.PreferencesJson)
	if err != nil {
		return nil, err
	}

	row, err := getProfileRow(req.ProfileId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "profile not found")
	}
	if err := setProfilePreferences(req.ProfileId, prefs); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}

	return &gen.ProfileResponse{
		ProfileId:       req.ProfileId,
		Name:            row.name,
		AvatarUrl:       row.avatarURL,
		PreferencesJson: prefs,
	}, nil
}

// normalizePrefsJSON validates a client-supplied preferences blob. Empty is
// coerced to "{}"; anything else must be well-formed JSON (the server keeps
// it opaque otherwise — the client owns the schema).
func normalizePrefsJSON(s string) (string, error) {
	if s == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(s)) {
		return "", status.Error(codes.InvalidArgument, "preferences_json must be valid JSON")
	}
	return s, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// RefreshToken — re-issues a 30-day JWT without requiring the PIN again.
// The caller must already hold a valid (non-expired) JWT.
// ─────────────────────────────────────────────────────────────────────────────

func (h *AuthHandler) RefreshToken(ctx context.Context, _ *gen.RefreshTokenRequest) (*gen.AuthorizeDeviceResponse, error) {
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(jwtTTL)
	tokenStr, err := h.mintJWT(deviceID, expiresAt)
	if err != nil {
		return nil, status.Error(codes.Internal, "jwt mint error")
	}
	log.Printf("[pileus/auth] device %s refreshed token", deviceID)
	return &gen.AuthorizeDeviceResponse{
		DeviceJwt: tokenStr,
		ExpiresAt: expiresAt.Unix(),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ListDevices / RenameDevice
// ─────────────────────────────────────────────────────────────────────────────

func (h *AuthHandler) ListDevices(ctx context.Context, _ *gen.ListDevicesRequest) (*gen.ListDevicesResponse, error) {
	if _, err := h.deviceFromCtx(ctx); err != nil {
		return nil, err
	}
	devices, err := listDevices()
	if err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	return &gen.ListDevicesResponse{Devices: devices}, nil
}

func (h *AuthHandler) RenameDevice(ctx context.Context, req *gen.RenameDeviceRequest) (*gen.RenameDeviceResponse, error) {
	caller, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if req.DeviceId == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id required")
	}
	// A device renames itself only; renaming others is the admin's job
	// (RenameDeviceAdmin, from the dashboard).
	if req.DeviceId != caller {
		return nil, status.Error(codes.PermissionDenied, "un dispositivo può rinominare solo sé stesso")
	}
	if err := renameDevice(req.DeviceId, sanitizeDeviceLabel(req.Label)); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	return &gen.RenameDeviceResponse{Ok: true}, nil
}

// RenameDeviceAdmin is the dashboard-side counterpart to the gRPC RenameDevice
// (which a Pileus client calls on itself): lets the admin give a device a
// human name ("TV salotto") from the device list instead of everyone having to
// recognise it by its opaque device_id. Returns the label actually stored
// (sanitizeDeviceLabel may have trimmed it).
func RenameDeviceAdmin(deviceID, label string) (string, error) {
	if deviceID == "" {
		return "", errors.New("device_id required")
	}
	clean := sanitizeDeviceLabel(label)
	if err := renameDevice(deviceID, clean); err != nil {
		return "", err
	}
	return clean, nil
}

// sanitizeDeviceLabel trims, caps, and strips control characters from a
// device label before it's stored — it's rendered back in the dashboard, and
// with the gRPC RenameDevice a device can suggest whatever string it wants
// for its own label (the DB write itself is already parametrised — this is
// about keeping the stored value sane, not SQL-safe).
func sanitizeDeviceLabel(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// JWT helpers
// ─────────────────────────────────────────────────────────────────────────────

type pileusJWTClaims struct {
	DeviceID string `json:"did"`
	jwt.RegisteredClaims
}

func (h *AuthHandler) mintJWT(deviceID string, exp time.Time) (string, error) {
	claims := pileusJWTClaims{
		DeviceID: deviceID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "mycelium",
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(h.jwtSecret)
}

// ParseJWT validates a Pileus JWT and returns the deviceID. Called by the
// gRPC interceptor to authenticate every call.
func (h *AuthHandler) ParseJWT(tokenStr string) (string, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &pileusJWTClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return h.jwtSecret, nil
	})
	if err != nil {
		return "", err
	}
	claims, ok := token.Claims.(*pileusJWTClaims)
	if !ok || !token.Valid {
		return "", errors.New("invalid token")
	}
	return claims.DeviceID, nil
}

// deviceFromCtx extracts and validates the Bearer JWT from gRPC metadata.
// Returns the deviceID on success.
func (h *AuthHandler) deviceFromCtx(ctx context.Context) (string, error) {
	deviceID, ok := ctx.Value(ctxKeyDeviceID{}).(string)
	if !ok || deviceID == "" {
		return "", status.Error(codes.Unauthenticated, "missing or invalid JWT")
	}
	return deviceID, nil
}

// ctxKeyDeviceID is the context key set by the auth interceptor.
type ctxKeyDeviceID struct{}

// ─────────────────────────────────────────────────────────────────────────────
// DB helpers — pileus_devices / pileus_profiles
// Tables are created by InitPileusDB(), called at startup.
// ─────────────────────────────────────────────────────────────────────────────

// deviceRevoked reports whether deviceID already has a row in pileus_devices
// and, if so, whether it's currently revoked. AuthorizeDevice calls this
// before upsertDevice so a fresh pairing code can't silently undo an admin's
// explicit revoke of an existing device (see the call site for why that
// matters — device_id is client-chosen, unrelated to the pairing code).
func deviceRevoked(deviceID string) (exists bool, revoked bool) {
	var revokedAt sql.NullString
	err := managers.DB.QueryRow(
		`SELECT revoked_at FROM pileus_devices WHERE device_id=?`,
		deviceID,
	).Scan(&revokedAt)
	if err != nil {
		return false, false
	}
	return true, revokedAt.Valid
}

func upsertDevice(deviceID string) error {
	// Clears any revoke flag on conflict — harmless today because
	// AuthorizeDevice already rejects a revoked existing device before calling
	// this (see deviceRevoked above), so this only ever runs for a brand new
	// device (revoked_at is NULL already) or an already-active one (ditto).
	_, err := managers.DB.Exec(
		`INSERT INTO pileus_devices(device_id, created_at)
		 VALUES(?, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id) DO UPDATE SET last_seen_at=CURRENT_TIMESTAMP, revoked_at=NULL`,
		deviceID,
	)
	if err == nil {
		forgetDeviceActive(deviceID)
	}
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Device revocation (P1-4). A 30-day Pileus JWT is otherwise unrevocable; the
// auth interceptor now also checks the device row still exists and isn't
// revoked, memoised briefly so a browsing burst doesn't hit SQLite per RPC.
// ─────────────────────────────────────────────────────────────────────────────

const deviceActiveCacheTTL = 60 * time.Second

type deviceActiveEntry struct {
	active bool
	at     time.Time
}

var (
	deviceActiveCache sync.Map // deviceID -> deviceActiveEntry

	// deviceActiveLookup is the DB check behind deviceActive; a package var so
	// tests can stub it without a live database.
	deviceActiveLookup = func(deviceID string) bool {
		var one int
		return managers.DB.QueryRow(
			`SELECT 1 FROM pileus_devices WHERE device_id=? AND revoked_at IS NULL`,
			deviceID,
		).Scan(&one) == nil
	}
)

// deviceActive reports whether deviceID is a known, non-revoked device.
func deviceActive(deviceID string) bool {
	if deviceID == "" {
		return false
	}
	if v, ok := deviceActiveCache.Load(deviceID); ok {
		if e := v.(deviceActiveEntry); time.Since(e.at) < deviceActiveCacheTTL {
			return e.active
		}
	}
	active := deviceActiveLookup(deviceID)
	deviceActiveCache.Store(deviceID, deviceActiveEntry{active: active, at: time.Now()})
	return active
}

func forgetDeviceActive(deviceID string) { deviceActiveCache.Delete(deviceID) }

// RevokeDevice invalidates every current token for deviceID without touching
// its profiles. Only an explicit admin action re-enables it (UnrevokeDevice /
// POST /admin/pileus/devices/{id}/unrevoke) — AuthorizeDevice deliberately
// refuses to resurrect a revoked device even with a fresh, valid pairing code
// (see the deviceRevoked check there): device_id is chosen by the client, not
// bound to the code, so a bare pairing code must not be enough to undo a
// revoke.
func RevokeDevice(deviceID string) error {
	_, err := managers.DB.Exec(
		`UPDATE pileus_devices SET revoked_at=CURRENT_TIMESTAMP
		 WHERE device_id=? AND revoked_at IS NULL`,
		deviceID,
	)
	if err == nil {
		forgetDeviceActive(deviceID)
	}
	return err
}

// UnrevokeDevice re-enables a revoked device (its unexpired tokens work again).
func UnrevokeDevice(deviceID string) error {
	_, err := managers.DB.Exec(
		`UPDATE pileus_devices SET revoked_at=NULL WHERE device_id=?`,
		deviceID,
	)
	if err == nil {
		forgetDeviceActive(deviceID)
	}
	return err
}

// DeleteDevice permanently forgets deviceID — a harder reset than RevokeDevice
// for a device that's gone for good (stolen, decommissioned TV, …) rather than
// just temporarily locked out. Safe for profiles: pileus_profiles.device_id is
// audit-only (see insertProfile) and the DB never runs with
// `PRAGMA foreign_keys=ON`, so the FK's ON DELETE CASCADE never fires — every
// profile (server-wide, shared across every paired device) is untouched. The
// device row itself is gone, so it drops off the dashboard list; if it ever
// reappears with a valid pairing code, AuthorizeDevice just re-inserts it.
func DeleteDevice(deviceID string) error {
	_, err := managers.DB.Exec(`DELETE FROM pileus_devices WHERE device_id=?`, deviceID)
	if err == nil {
		forgetDeviceActive(deviceID)
	}
	return err
}

// AdminDeviceInfo is the dashboard view of a paired device (adds `revoked`).
type AdminDeviceInfo struct {
	DeviceID   string `json:"device_id"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	LastSeenAt int64  `json:"last_seen_at"`
	Revoked    bool   `json:"revoked"`
}

// ListDevicesAdmin returns every paired device with its revoke state.
func ListDevicesAdmin() ([]AdminDeviceInfo, error) {
	rows, err := managers.DB.Query(
		`SELECT device_id, COALESCE(label,''),
		        COALESCE(strftime('%s',created_at),'0'),
		        COALESCE(strftime('%s',last_seen_at),'0'),
		        revoked_at IS NOT NULL
		 FROM pileus_devices ORDER BY created_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminDeviceInfo{}
	for rows.Next() {
		var d AdminDeviceInfo
		if err := rows.Scan(&d.DeviceID, &d.Label, &d.CreatedAt, &d.LastSeenAt, &d.Revoked); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// insertProfile records the creating device in device_id purely for audit —
// profiles are server-wide (see listProfiles), not scoped to it. prefsJSON is
// an opaque, already-validated blob (see normalizePrefsJSON).
func insertProfile(deviceID, profileID, name, avatarURL string, prefsJSON string) error {
	_, err := managers.DB.Exec(
		`INSERT INTO pileus_profiles(profile_id, device_id, name, avatar_url, preferences, created_at)
		 VALUES(?,?,?,?,?,CURRENT_TIMESTAMP)`,
		profileID, deviceID, name, avatarURL, prefsJSON,
	)
	return err
}

// listProfiles returns every profile on this server. A mycelium instance is a
// single trust domain (one owner/family, shared rotating pairing code), so a
// device paired to it may see and use any profile — pairing a new device
// finds the profiles the others already created.
func listProfiles() ([]*gen.ProfileResponse, error) {
	rows, err := managers.DB.Query(
		`SELECT profile_id, name, avatar_url, preferences
		 FROM pileus_profiles ORDER BY created_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*gen.ProfileResponse
	for rows.Next() {
		p := &gen.ProfileResponse{}
		if err := rows.Scan(&p.ProfileId, &p.Name, &p.AvatarUrl, &p.PreferencesJson); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// setProfilePreferences replaces the opaque preferences blob for one profile.
func setProfilePreferences(profileID, prefsJSON string) error {
	_, err := managers.DB.Exec(
		`UPDATE pileus_profiles SET preferences=? WHERE profile_id=?`,
		prefsJSON, profileID,
	)
	return err
}

func deleteProfile(profileID string) error {
	_, err := managers.DB.Exec(
		`DELETE FROM pileus_profiles WHERE profile_id=?`,
		profileID,
	)
	if err != nil {
		return err
	}
	// The profile's plugin logins go with it.
	return managers.DB.DeletePluginSecretsForProfile(profileID)
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// profileRow holds the current DB values for UpdateProfile / SetProfilePreferences.
type profileRow struct {
	name        string
	avatarURL   string
	preferences string
}

func getProfileRow(profileID string) (*profileRow, error) {
	row := &profileRow{}
	err := managers.DB.QueryRow(
		`SELECT name, avatar_url, preferences
		 FROM pileus_profiles WHERE profile_id=?`,
		profileID,
	).Scan(&row.name, &row.avatarURL, &row.preferences)
	if err != nil {
		return nil, err
	}
	return row, nil
}

// profileExists reports whether profileID names a profile on this server.
// Guards every RPC that accepts a client-supplied profile_id (the
// x-profile-id header, or a request field) before touching per-profile data
// — profiles are server-wide, so the real boundary is "is the caller a
// paired device" (JWT), enforced by the auth interceptor; this only rejects
// a stale or bogus profile_id.
func profileExists(profileID string) bool {
	if profileID == "" {
		return false
	}
	var one int
	err := managers.DB.QueryRow(
		`SELECT 1 FROM pileus_profiles WHERE profile_id=?`,
		profileID,
	).Scan(&one)
	return err == nil
}

func updateProfile(profileID, name, avatarURL string) error {
	_, err := managers.DB.Exec(
		`UPDATE pileus_profiles
		 SET name=?, avatar_url=?
		 WHERE profile_id=?`,
		name, avatarURL, profileID,
	)
	return err
}

func listDevices() ([]*gen.DeviceInfo, error) {
	rows, err := managers.DB.Query(
		`SELECT device_id, COALESCE(label,''), strftime('%s',created_at), strftime('%s',last_seen_at)
		 FROM pileus_devices ORDER BY created_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*gen.DeviceInfo
	for rows.Next() {
		d := &gen.DeviceInfo{}
		if err := rows.Scan(&d.DeviceId, &d.Label, &d.CreatedAt, &d.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func renameDevice(deviceID, label string) error {
	_, err := managers.DB.Exec(
		`UPDATE pileus_devices SET label=? WHERE device_id=?`,
		label, deviceID,
	)
	return err
}
