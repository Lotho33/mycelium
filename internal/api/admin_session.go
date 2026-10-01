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
)

// --- Admin sessions ---
//
// Self-verifying cookie (id.expiry.hmac): nothing server-side is needed to
// validate it, so restarts don't log the admin out. Logout adds the id to an
// in-memory denylist until the cookie would expire.

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

func signAdminSession(id string, exp int64) string {
	mac := hmac.New(sha256.New, adminSessionKey)
	fmt.Fprintf(mac, "%s.%d", id, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// createAdminSession mints a new self-verifying token: <random-id>.<exp>.<hmac>.
func createAdminSession() string {
	id := generateSessionToken()
	exp := time.Now().Add(adminSessionTTL).Unix()
	return fmt.Sprintf("%s.%d.%s", id, exp, signAdminSession(id, exp))
}

func isValidAdminSession(token string) bool {
	if len(adminSessionKey) == 0 {
		return false // never initialised — fail closed rather than accept anything
	}
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return false
	}
	id, expStr, sig := parts[0], parts[1], parts[2]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(signAdminSession(id, exp)), []byte(sig)) != 1 {
		return false
	}
	if time.Now().Unix() >= exp {
		return false
	}
	return !isRevokedAdminSession(id)
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

func revokeAdminSession(token string) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return
	}
	id := parts[0]
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		exp = time.Now().Add(adminSessionTTL).Unix() // fallback: denylist for a full TTL
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
	}
}
