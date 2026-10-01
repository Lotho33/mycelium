// Package pileus implements the gRPC server the Pileus client apps talk to:
// device pairing, profiles, media pipeline and plugin actions.
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

// NewAuthHandler returns an AuthHandler. jwtSecret must be stable across
// restarts (use DeriveJWTSecret); it panics when empty.
func NewAuthHandler(jwtSecret []byte) *AuthHandler {
	if len(jwtSecret) == 0 {
		panic("pileus: jwt secret must not be empty")
	}
	return &AuthHandler{jwtSecret: jwtSecret}
}

// DeriveJWTSecret derives the device-JWT signing key from the master secret
// ("pileus_jwt_secret") with HMAC-SHA256 and a domain-separation constant,
// like the proxy-URL and admin-session keys: one master, a distinct key per
// use.
func DeriveJWTSecret(master []byte) []byte {
	if len(master) == 0 {
		return nil
	}
	m := hmac.New(sha256.New, master)
	m.Write([]byte("mycelium/jwt/v1"))
	return m.Sum(nil)
}

func (h *AuthHandler) AuthorizeDevice(ctx context.Context, req *gen.AuthorizeDeviceRequest) (*gen.AuthorizeDeviceResponse, error) {
	if req.DeviceId == "" || req.PinHash == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id and pin_hash required")
	}

	// req.PinHash carries the pairing code typed in the client (the field name
	// is kept for wire compatibility): a short-lived, single-use code generated
	// from the dashboard (pairing.go).
	//
	// device_id is chosen by the client and is also the owner key of a
	// profile-less device's history and downloads: validate it, and refuse one
	// equal to a profile id, before the code is spent.
	if !validDeviceID(req.DeviceId) || profileExists(req.DeviceId) {
		return nil, status.Error(codes.InvalidArgument, "device_id non valido")
	}
	if !VerifyPairingCode(req.PinHash, req.DeviceId) {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired pairing code")
	}

	// A pairing code never re-enables a revoked device: device ids are chosen
	// by the client, so re-enabling is an explicit admin action (UnrevokeDevice).
	if exists, revoked := deviceRevoked(req.DeviceId); exists && revoked {
		return nil, status.Error(codes.PermissionDenied, "device revoked; ask an admin to re-enable it from the dashboard")
	}

	// Re-pairing an existing id can't prove it's the same device: drop its PIN
	// trusts and sessions, so a pairing code never inherits another device's
	// access to protected profiles.
	if exists, _ := deviceRevoked(req.DeviceId); exists {
		forgetDeviceAccess(req.DeviceId)
		managers.PlaybackLeases.ReleaseDevice(req.DeviceId)
		log.Printf("[pileus/auth] device %s ri-associato: fiducie PIN azzerate", req.DeviceId)
	}

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

// validDeviceID bounds a client-chosen device id: 1-128 printable ASCII
// characters.
func validDeviceID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

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
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	profiles, err := listProfiles()
	if err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	for _, p := range profiles {
		decorateProfile(ctx, deviceID, p)
	}
	return &gen.ListProfilesResponse{Profiles: profiles}, nil
}

func (h *AuthHandler) DeleteProfile(ctx context.Context, req *gen.DeleteProfileRequest) (*gen.DeleteProfileResponse, error) {
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if req.ProfileId == "" {
		return nil, status.Error(codes.InvalidArgument, "profile_id required")
	}
	// A PIN-protected profile can't be deleted by someone who can't open it.
	if err := requireProfileAccess(ctx, deviceID, req.ProfileId); err != nil {
		return nil, err
	}
	if err := deleteProfile(req.ProfileId); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	return &gen.DeleteProfileResponse{Ok: true}, nil
}

func (h *AuthHandler) UpdateProfile(ctx context.Context, req *gen.UpdateProfileRequest) (*gen.ProfileResponse, error) {
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if req.ProfileId == "" {
		return nil, status.Error(codes.InvalidArgument, "profile_id required")
	}
	if err := requireProfileAccess(ctx, deviceID, req.ProfileId); err != nil {
		return nil, err
	}

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
	// UpdateProfile never touches preferences: echo the stored blob.
	resp := &gen.ProfileResponse{
		ProfileId:       req.ProfileId,
		Name:            name,
		AvatarUrl:       avatarURL,
		PreferencesJson: row.preferences,
	}
	decorateProfile(ctx, deviceID, resp)
	return resp, nil
}

// SetProfilePreferences replaces the profile's opaque preferences blob
// (profiles are server-wide, so it follows the person to every device).

func (h *AuthHandler) SetProfilePreferences(ctx context.Context, req *gen.SetProfilePreferencesRequest) (*gen.ProfileResponse, error) {
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if req.ProfileId == "" {
		return nil, status.Error(codes.InvalidArgument, "profile_id required")
	}
	if err := requireProfileAccess(ctx, deviceID, req.ProfileId); err != nil {
		return nil, err
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

	resp := &gen.ProfileResponse{
		ProfileId:       req.ProfileId,
		Name:            row.name,
		AvatarUrl:       row.avatarURL,
		PreferencesJson: prefs,
	}
	decorateProfile(ctx, deviceID, resp)
	return resp, nil
}

// normalizePrefsJSON validates a client-supplied preferences blob: "" becomes
// "{}", anything else must be valid JSON (the client owns the schema).
func normalizePrefsJSON(s string) (string, error) {
	if s == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(s)) {
		return "", status.Error(codes.InvalidArgument, "preferences_json must be valid JSON")
	}
	return s, nil
}

// RefreshToken re-issues a 30-day JWT to a device holding a valid one.

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
	// A device renames only itself; the admin renames any (RenameDeviceAdmin).
	if req.DeviceId != caller {
		return nil, status.Error(codes.PermissionDenied, "un dispositivo può rinominare solo sé stesso")
	}
	if err := renameDevice(req.DeviceId, sanitizeDeviceLabel(req.Label)); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	return &gen.RenameDeviceResponse{Ok: true}, nil
}

// UnpairSelf lets a device remove itself (e.g. when the client switches to
// another server). The id always comes from the caller's JWT.
func (h *AuthHandler) UnpairSelf(ctx context.Context, _ *gen.UnpairSelfRequest) (*gen.UnpairSelfResponse, error) {
	deviceID, err := h.deviceFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if err := DeleteDevice(deviceID); err != nil {
		return nil, status.Error(codes.Internal, "db error: "+err.Error())
	}
	log.Printf("[pileus/auth] device %s si è smarcato (cambio server)", deviceID)
	return &gen.UnpairSelfResponse{Ok: true}, nil
}

// RenameDeviceAdmin sets a device's label from the dashboard and returns
// the label actually stored.
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

// sanitizeDeviceLabel trims, caps (64 bytes) and strips control characters
// from a device label.
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

// ParseJWT validates a device JWT and returns its deviceID.
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

// deviceFromCtx returns the deviceID the auth interceptor stored in ctx.
func (h *AuthHandler) deviceFromCtx(ctx context.Context) (string, error) {
	deviceID, ok := ctx.Value(ctxKeyDeviceID{}).(string)
	if !ok || deviceID == "" {
		return "", status.Error(codes.Unauthenticated, "missing or invalid JWT")
	}
	return deviceID, nil
}

// ctxKeyDeviceID is the context key set by the auth interceptor.
type ctxKeyDeviceID struct{}

// deviceRevoked reports whether deviceID has a row in pileus_devices and
// whether it is revoked.
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
	// Clears the revoke flag on conflict: AuthorizeDevice has already refused a
	// revoked device before getting here.
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
// Device revocation: the auth interceptor checks that the device row exists
// and isn't revoked, memoised briefly so a burst of RPCs doesn't hit SQLite
// each time.
// ─────────────────────────────────────────────────────────────────────────────

const deviceActiveCacheTTL = 60 * time.Second

type deviceActiveEntry struct {
	active bool
	at     time.Time
}

var (
	deviceActiveCache sync.Map // deviceID -> deviceActiveEntry

	// deviceActiveLookup is the DB check behind deviceActive (a var for tests).
	deviceActiveLookup = func(deviceID string) bool {
		var one int
		return managers.DB.QueryRow(
			`SELECT 1 FROM pileus_devices WHERE device_id=? AND revoked_at IS NULL`,
			deviceID,
		).Scan(&one) == nil
	}

	// deviceSeen records that deviceID made an authenticated call (at most once
	// per deviceActiveCacheTTL). A var for tests.
	deviceSeen = func(deviceID string) {
		if managers.DB == nil {
			return
		}
		_, _ = managers.DB.Exec(`UPDATE pileus_devices SET last_seen_at=CURRENT_TIMESTAMP WHERE device_id=?`, deviceID)
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
	if active {
		deviceSeen(deviceID)
	}
	return active
}

func forgetDeviceActive(deviceID string) { deviceActiveCache.Delete(deviceID) }

// RevokeDevice invalidates every token of deviceID, keeping its profiles.
// Only UnrevokeDevice re-enables it.
func RevokeDevice(deviceID string) error {
	_, err := managers.DB.Exec(
		`UPDATE pileus_devices SET revoked_at=CURRENT_TIMESTAMP
		 WHERE device_id=? AND revoked_at IS NULL`,
		deviceID,
	)
	if err == nil {
		forgetDeviceActive(deviceID)
		managers.PlaybackLeases.ReleaseDevice(deviceID)
		forgetDeviceAccess(deviceID)
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

// DeleteDevice permanently forgets deviceID. Profiles are untouched
// (pileus_profiles.device_id is audit-only); a valid pairing code can
// re-add the device later.
func DeleteDevice(deviceID string) error {
	_, err := managers.DB.Exec(`DELETE FROM pileus_devices WHERE device_id=?`, deviceID)
	if err == nil {
		forgetDeviceActive(deviceID)
		managers.PlaybackLeases.ReleaseDevice(deviceID)
		forgetDeviceAccess(deviceID)
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

// insertProfile records the creating device for audit only: profiles are
// server-wide. prefsJSON is an already-validated blob.
func insertProfile(deviceID, profileID, name, avatarURL string, prefsJSON string) error {
	_, err := managers.DB.Exec(
		`INSERT INTO pileus_profiles(profile_id, device_id, name, avatar_url, preferences, created_at)
		 VALUES(?,?,?,?,?,CURRENT_TIMESTAMP)`,
		profileID, deviceID, name, avatarURL, prefsJSON,
	)
	return err
}

// listProfiles returns every profile on this server: an instance is a single
// trust domain, so every paired device sees every profile (PIN-protected
// ones need unlocking).
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
	// PIN trusts/sessions and the profile's plugin secrets go with it.
	forgetProfileAccess(profileID)
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

// deviceLabel is deviceID's dashboard label, "" when unset or unknown.
func deviceLabel(deviceID string) string {
	if managers.DB == nil {
		return ""
	}
	var label string
	_ = managers.DB.QueryRow(
		`SELECT COALESCE(label,'') FROM pileus_devices WHERE device_id=?`,
		deviceID,
	).Scan(&label)
	return label
}

func renameDevice(deviceID, label string) error {
	_, err := managers.DB.Exec(
		`UPDATE pileus_devices SET label=? WHERE device_id=?`,
		label, deviceID,
	)
	return err
}
