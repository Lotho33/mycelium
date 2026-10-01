package pileus

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"mycelium/internal/managers"
)

// ─────────────────────────────────────────────────────────────────────────────
// Profile PIN
//
// A profile may carry a 4-8 digit PIN. A protected profile can only be used
// by a device that is either
//   - trusted for it (pileus_profile_trust): the PIN was entered once with
//     "remember this device"; or
//   - holding a one-off session token (UnlockProfile without remember), kept
//     in the client's memory and sent as x-profile-session.
// The check runs in the gRPC auth interceptor (profileAccessible).
// ─────────────────────────────────────────────────────────────────────────────

const (
	pinMinLen = 4
	pinMaxLen = 8

	// One-off sessions are bounded server-side too (a token left behind by a
	// crashed app, or a stolen one).
	profileSessionIdle = 4 * time.Hour
	profileSessionMax  = 24 * time.Hour
)

var (
	errPinFormat = errors.New("il PIN deve essere di 4-8 cifre")
	errPinWrong  = errors.New("PIN errato")
)

func validPinFormat(pin string) bool {
	if len(pin) < pinMinLen || len(pin) > pinMaxLen {
		return false
	}
	for _, c := range pin {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func hashPin(pin string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
	return string(b), err
}

// ─── DB ──────────────────────────────────────────────────────────────────────

// profilePinHash returns the profile's PIN hash, "" when it has none (or
// doesn't exist). The legacy pin_hash column is unused.
func profilePinHash(profileID string) (string, error) {
	var h sql.NullString
	err := managers.DB.QueryRow(
		`SELECT access_pin_hash FROM pileus_profiles WHERE profile_id=?`, profileID,
	).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return h.String, nil
}

func storeProfilePinHash(profileID, hash string) error {
	var v any
	if hash != "" {
		v = hash
	}
	res, err := managers.DB.Exec(`UPDATE pileus_profiles SET access_pin_hash=? WHERE profile_id=?`, v, profileID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	forgetPinCache(profileID)
	return nil
}

func isDeviceTrusted(deviceID, profileID string) bool {
	var one int
	return managers.DB.QueryRow(
		`SELECT 1 FROM pileus_profile_trust WHERE device_id=? AND profile_id=?`, deviceID, profileID,
	).Scan(&one) == nil
}

func trustDevice(deviceID, profileID string) error {
	_, err := managers.DB.Exec(
		`INSERT INTO pileus_profile_trust(device_id, profile_id, created_at) VALUES(?,?,CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id, profile_id) DO NOTHING`, deviceID, profileID)
	forgetPinCache(profileID)
	return err
}

func untrustDevice(deviceID, profileID string) error {
	_, err := managers.DB.Exec(`DELETE FROM pileus_profile_trust WHERE device_id=? AND profile_id=?`, deviceID, profileID)
	forgetPinCache(profileID)
	return err
}

func untrustProfile(profileID string) error {
	_, err := managers.DB.Exec(`DELETE FROM pileus_profile_trust WHERE profile_id=?`, profileID)
	forgetPinCache(profileID)
	return err
}

func untrustDeviceEverywhere(deviceID string) error {
	_, err := managers.DB.Exec(`DELETE FROM pileus_profile_trust WHERE device_id=?`, deviceID)
	clearPinCache()
	return err
}

// ─── access cache ────────────────────────────────────────────────────────────
// profileAccessible runs on every RPC with a profile id: memoise its two
// lookups briefly. Every write here invalidates the cache.

const pinCacheTTL = 60 * time.Second

type pinCacheEntry struct {
	protected bool
	trusted   map[string]bool // deviceID -> trusted
	at        time.Time
}

var (
	pinCacheMu sync.Mutex
	pinCache   = map[string]*pinCacheEntry{} // profileID ->
)

func forgetPinCache(profileID string) {
	pinCacheMu.Lock()
	delete(pinCache, profileID)
	pinCacheMu.Unlock()
}

func clearPinCache() {
	pinCacheMu.Lock()
	pinCache = map[string]*pinCacheEntry{}
	pinCacheMu.Unlock()
}

// profileProtected reports whether profileID has a PIN.
func profileProtected(profileID string) (bool, error) {
	pinCacheMu.Lock()
	if e := pinCache[profileID]; e != nil && time.Since(e.at) < pinCacheTTL {
		pinCacheMu.Unlock()
		return e.protected, nil
	}
	pinCacheMu.Unlock()
	h, err := profilePinHash(profileID)
	if err != nil {
		return false, err
	}
	pinCacheMu.Lock()
	pinCache[profileID] = &pinCacheEntry{protected: h != "", trusted: map[string]bool{}, at: time.Now()}
	pinCacheMu.Unlock()
	return h != "", nil
}

func deviceTrustedCached(deviceID, profileID string) bool {
	pinCacheMu.Lock()
	e := pinCache[profileID]
	if e != nil && time.Since(e.at) < pinCacheTTL {
		if v, ok := e.trusted[deviceID]; ok {
			pinCacheMu.Unlock()
			return v
		}
	}
	pinCacheMu.Unlock()
	v := isDeviceTrusted(deviceID, profileID)
	pinCacheMu.Lock()
	if e := pinCache[profileID]; e != nil {
		e.trusted[deviceID] = v
	}
	pinCacheMu.Unlock()
	return v
}

// profileAccessible reports whether deviceID may use profileID now: no PIN,
// a trusted device, or a live one-off session of this device+profile. An
// unknown profile counts as accessible (handlers answer NotFound).
func profileAccessible(deviceID, profileID, sessionToken string) (bool, error) {
	protected, err := profileProtected(profileID)
	if err != nil || !protected {
		return !protected, err
	}
	if deviceTrustedCached(deviceID, profileID) {
		return true, nil
	}
	return profileSessions.valid(sessionToken, deviceID, profileID), nil
}

// ─── one-off sessions (memory only) ──────────────────────────────────────────

type profileSession struct {
	deviceID, profileID string
	created, lastUsed   time.Time
}

type profileSessionStore struct {
	mu  sync.Mutex
	m   map[string]*profileSession
	now func() time.Time
}

var profileSessions = &profileSessionStore{m: map[string]*profileSession{}, now: time.Now}

func (s *profileSessionStore) create(deviceID, profileID string) (token string, expires time.Time, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	token = hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.gcLocked(now)
	s.m[token] = &profileSession{deviceID: deviceID, profileID: profileID, created: now, lastUsed: now}
	return token, now.Add(profileSessionMax), nil
}

// valid checks token for deviceID+profileID and slides its idle expiry.
func (s *profileSessionStore) valid(token, deviceID, profileID string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ps := s.m[token]
	if ps == nil {
		return false
	}
	now := s.now()
	if now.Sub(ps.lastUsed) > profileSessionIdle || now.Sub(ps.created) > profileSessionMax {
		delete(s.m, token)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(ps.deviceID), []byte(deviceID)) != 1 || ps.profileID != profileID {
		return false
	}
	ps.lastUsed = now
	return true
}

// drop removes sessions matching profileID (and deviceID when non-empty).
// profileID "" matches every profile.
func (s *profileSessionStore) drop(profileID, deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, ps := range s.m {
		if (profileID == "" || ps.profileID == profileID) && (deviceID == "" || ps.deviceID == deviceID) {
			delete(s.m, t)
		}
	}
}

func (s *profileSessionStore) gcLocked(now time.Time) {
	for t, ps := range s.m {
		if now.Sub(ps.lastUsed) > profileSessionIdle || now.Sub(ps.created) > profileSessionMax {
			delete(s.m, t)
		}
	}
}

// ─── wrong-PIN limiter ───────────────────────────────────────────────────────
// Per device+profile: 5 misses, then a lockout doubling from 1 min to 1 h.
// Per profile across devices: 20 misses within an hour lock it for 15 min.

const (
	pinFreeMisses      = 5
	pinBaseLockout     = time.Minute
	pinMaxLockout      = time.Hour
	pinProfileMisses   = 20
	pinProfileWindow   = time.Hour
	pinProfileLockout  = 15 * time.Minute
	pinLimiterMaxEntry = 4096
)

type pinMissState struct {
	misses      int
	lockedUntil time.Time
	windowStart time.Time
	last        time.Time
}

type pinLimiter struct {
	mu  sync.Mutex
	m   map[string]*pinMissState
	now func() time.Time
}

var pinAttempts = &pinLimiter{m: map[string]*pinMissState{}, now: time.Now}

func (l *pinLimiter) keys(deviceID, profileID string) (dev, prof string) {
	return "d\x00" + deviceID + "\x00" + profileID, "p\x00" + profileID
}

// lockedFor returns how long the caller must still wait (0 = may try).
func (l *pinLimiter) lockedFor(deviceID, profileID string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var wait time.Duration
	dk, pk := l.keys(deviceID, profileID)
	for _, k := range []string{dk, pk} {
		if st := l.m[k]; st != nil && st.lockedUntil.After(now) {
			if d := st.lockedUntil.Sub(now); d > wait {
				wait = d
			}
		}
	}
	return wait
}

func (l *pinLimiter) fail(deviceID, profileID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.m) > pinLimiterMaxEntry {
		for k, st := range l.m {
			if now.Sub(st.last) > 24*time.Hour {
				delete(l.m, k)
			}
		}
	}
	dk, pk := l.keys(deviceID, profileID)

	d := l.m[dk]
	if d == nil {
		d = &pinMissState{}
		l.m[dk] = d
	}
	d.misses++
	d.last = now
	if d.misses >= pinFreeMisses {
		lock := pinBaseLockout << (d.misses - pinFreeMisses)
		if lock > pinMaxLockout || lock <= 0 {
			lock = pinMaxLockout
		}
		d.lockedUntil = now.Add(lock)
	}

	p := l.m[pk]
	if p == nil || now.Sub(p.windowStart) > pinProfileWindow {
		p = &pinMissState{windowStart: now}
		l.m[pk] = p
	}
	p.misses++
	p.last = now
	if p.misses >= pinProfileMisses {
		p.lockedUntil = now.Add(pinProfileLockout)
		p.misses = 0
		p.windowStart = now
	}
}

func (l *pinLimiter) success(deviceID, profileID string) {
	l.mu.Lock()
	dk, _ := l.keys(deviceID, profileID)
	delete(l.m, dk)
	l.mu.Unlock()
}

// ─── operations shared by gRPC and the dashboard ─────────────────────────────

// pinVerifyLocks serialises PIN checks per profile: the limiter learns about
// a miss only after bcrypt, so concurrent guesses would otherwise all pass
// lockedFor() first. Queued guesses behind a lockout return at once.
var pinVerifyLocks sync.Map // profileID -> *sync.Mutex

func pinVerifyLock(profileID string) *sync.Mutex {
	mu, _ := pinVerifyLocks.LoadOrStore(profileID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// verifyProfilePin checks pin against profileID's hash under the limiter.
// Returns (wait>0, nil) while locked out.
func verifyProfilePin(deviceID, profileID, pin string) (wait time.Duration, err error) {
	mu := pinVerifyLock(profileID)
	mu.Lock()
	defer mu.Unlock()
	if w := pinAttempts.lockedFor(deviceID, profileID); w > 0 {
		return w, nil
	}
	h, err := profilePinHash(profileID)
	if err != nil {
		return 0, err
	}
	if h == "" {
		return 0, nil
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte(pin)) != nil {
		pinAttempts.fail(deviceID, profileID)
		return 0, errPinWrong
	}
	pinAttempts.success(deviceID, profileID)
	return 0, nil
}

// replaceProfilePin sets (or with newPin "" removes) the PIN and revokes all
// of the profile's trusted devices and one-off sessions.
func replaceProfilePin(profileID, newPin string) error {
	hash := ""
	if newPin != "" {
		if !validPinFormat(newPin) {
			return errPinFormat
		}
		var err error
		if hash, err = hashPin(newPin); err != nil {
			return err
		}
	}
	if err := storeProfilePinHash(profileID, hash); err != nil {
		return err
	}
	profileSessions.drop(profileID, "")
	return untrustProfile(profileID)
}

// forgetProfileAccess drops everything tied to a deleted profile.
func forgetProfileAccess(profileID string) {
	profileSessions.drop(profileID, "")
	_ = untrustProfile(profileID)
}

// forgetDeviceAccess drops a revoked/deleted device's trusts and sessions.
func forgetDeviceAccess(deviceID string) {
	profileSessions.drop("", deviceID)
	_ = untrustDeviceEverywhere(deviceID)
}

// ResetAllProfileAccess forgets every one-off session and trust (profile
// wipe, factory reset).
func ResetAllProfileAccess() {
	profileSessions.drop("", "")
	clearPinCache()
}

func lockoutMessage(wait time.Duration) string {
	secs := int(wait.Round(time.Second).Seconds())
	if secs < 60 {
		return fmt.Sprintf("troppi PIN errati: riprova tra %d secondi", secs)
	}
	return fmt.Sprintf("troppi PIN errati: riprova tra %d minuti", (secs+59)/60)
}

// ─── dashboard ───────────────────────────────────────────────────────────────

// TrustedDeviceInfo is one device trusted for a profile (dashboard view).
type TrustedDeviceInfo struct {
	DeviceID  string `json:"device_id"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"created_at"`
}

func listTrustedDevices(profileID string) ([]TrustedDeviceInfo, error) {
	rows, err := managers.DB.Query(
		`SELECT t.device_id, COALESCE(d.label,''), COALESCE(strftime('%s',t.created_at),'0')
		 FROM pileus_profile_trust t LEFT JOIN pileus_devices d ON d.device_id=t.device_id
		 WHERE t.profile_id=? ORDER BY t.created_at`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrustedDeviceInfo{}
	for rows.Next() {
		var t TrustedDeviceInfo
		if err := rows.Scan(&t.DeviceID, &t.Label, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetProfilePinAdmin sets or (pin "") removes a profile's PIN from the
// dashboard, without the current PIN: the admin is the recovery path.
func SetProfilePinAdmin(profileID, pin string) error {
	return replaceProfilePin(strings.TrimSpace(profileID), pin)
}

// UntrustDeviceAdmin revokes one device's trust for a profile.
func UntrustDeviceAdmin(profileID, deviceID string) error {
	profileSessions.drop(profileID, deviceID)
	return untrustDevice(deviceID, profileID)
}

// IsPinFormatError reports whether err is the "4-8 digits" validation error.
func IsPinFormatError(err error) bool { return errors.Is(err, errPinFormat) }
