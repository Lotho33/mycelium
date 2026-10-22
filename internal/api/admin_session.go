package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// --- Admin sessions ---
//
// Self-verifying cookie (id.expiry.hmac): nothing server-side is needed to
// validate it, so restarts don't log the admin out. The HMAC also covers the
// current admin password hash, so a password change or a factory reset
// invalidates every outstanding session. Logout adds the id to an in-memory
// denylist until the cookie would expire.

const adminSessionCookie = "mycelium_admin_session"
const adminSessionTTL = 12 * time.Hour

// adminSessionKey signs the cookie; nil until SetAdminSessionKey runs at
// startup (cmd/server/main.go, derived from the same master secret as the
// Pileus JWT / the /proxy URL signature — a different subkey per use, no
// shared key material across the three).
var adminSessionKey []byte

// SetAdminSessionKey installs the signing key, derived from master. Call once
// at startup.
func SetAdminSessionKey(master []byte) {
	if len(master) == 0 {
		adminSessionKey = nil
		return
	}
	m := hmac.New(sha256.New, master)
	m.Write([]byte("mycelium/admin-session/v1"))
	adminSessionKey = m.Sum(nil)
}

func generateSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand non disponibile: " + err.Error())
	}
	return base64.URLEncoding.EncodeToString(b)
}

// signAdminSession binds the token to the admin password hash in force when
// it was minted (see the package comment above).
func signAdminSession(id string, exp int64) string {
	mac := hmac.New(sha256.New, adminSessionKey)
	fmt.Fprintf(mac, "%s.%d.%s", id, exp, managers.Settings.MasterAdminHash())
	return hex.EncodeToString(mac.Sum(nil))
}

// createAdminSession mints a new self-verifying token: <random-id>.<exp>.<hmac>.
func createAdminSession() string {
	id := generateSessionToken()
	exp := time.Now().Add(adminSessionTTL).Unix()
	return fmt.Sprintf("%s.%d.%s", id, exp, signAdminSession(id, exp))
}

// parseAdminSession returns the id and expiry of a correctly signed, unexpired
// token (revocation not checked).
func parseAdminSession(token string) (id string, exp int64, ok bool) {
	if len(adminSessionKey) == 0 || !managers.Settings.IsSetupDone() {
		return "", 0, false // not initialised / no admin yet — fail closed
	}
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return "", 0, false
	}
	id, expStr, sig := parts[0], parts[1], parts[2]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", 0, false
	}
	if subtle.ConstantTimeCompare([]byte(signAdminSession(id, exp)), []byte(sig)) != 1 {
		return "", 0, false
	}
	if time.Now().Unix() >= exp {
		return "", 0, false
	}
	return id, exp, true
}

func isValidAdminSession(token string) bool {
	id, _, ok := parseAdminSession(token)
	return ok && !isRevokedAdminSession(id)
}

// revokedAdminSessions denylists a session id (not the whole token — no need
// to keep the signature around) from an explicit logout until it would have
// expired naturally anyway.
var (
	revokedAdminSessions   = make(map[string]int64) // id -> expiry (unix)
	revokedAdminSessionsMu sync.Mutex
)

func isRevokedAdminSession(id string) bool {
	revokedAdminSessionsMu.Lock()
	defer revokedAdminSessionsMu.Unlock()
	_, revoked := revokedAdminSessions[id]
	return revoked
}

func init() {
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			func() {
				defer core.Guard("api/admin-session-gc-tick")
				now := time.Now().Unix()
				revokedAdminSessionsMu.Lock()
				defer revokedAdminSessionsMu.Unlock()
				for id, exp := range revokedAdminSessions {
					if now >= exp {
						delete(revokedAdminSessions, id)
					}
				}
			}()
		}
	}()
}

// revokeAdminSession denylists token. Only a genuine session is recorded:
// logout is unauthenticated, so anything else would let a client grow the
// denylist at will.
func revokeAdminSession(token string) {
	id, exp, ok := parseAdminSession(token)
	if !ok {
		return
	}
	if max := time.Now().Add(adminSessionTTL).Unix(); exp > max {
		exp = max
	}
	revokedAdminSessionsMu.Lock()
	revokedAdminSessions[id] = exp
	revokedAdminSessionsMu.Unlock()
}

// adminAuthMiddleware requires a valid admin session. API requests
// (Accept: application/json or text/event-stream) get 401, others are
// redirected to the login page.
func adminAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(adminSessionCookie)
		if err != nil || !isValidAdminSession(cookie.Value) {
			accept := r.Header.Get("Accept")
			if strings.Contains(accept, "application/json") ||
				strings.Contains(accept, "text/event-stream") ||
				r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
				http.Error(w, `{"detail":"non autorizzato"}`, http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next(w, r)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Live dashboards: show the write's effect now (admin_events.go).
			adminEvents.Poke()
		}
	}
}
