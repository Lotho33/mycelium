// Device pairing with a short-lived rotating code generated from the admin
// dashboard and typed once into the client: short (typeable on a TV
// remote), single-use, expiring in minutes, and unrelated to the admin
// password.
package pileus

import (
	"crypto/rand"
	"crypto/subtle"
	"os"
	"strings"
	"sync"
	"time"
)

// codeAlphabet excludes ambiguous characters (0/O, 1/I/L).
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

const (
	pairingCodeLen  = 6
	pairingTTL      = 5 * time.Minute
	pairingMaxTries = 5 // wrong attempts against one code before it's burned
)

type pairingState struct {
	code      string
	expiresAt time.Time
	used      bool
	tries     int
	deviceID  string
}

var (
	pairingMu      sync.Mutex
	currentPairing *pairingState
)

// GeneratePairingCode mints a new code, replacing any pending one (one code
// at a time). Dashboard only.
func GeneratePairingCode() (code string, expiresAt time.Time) {
	code = randomCode(pairingCodeLen)
	expiresAt = time.Now().Add(pairingTTL)
	pairingMu.Lock()
	currentPairing = &pairingState{code: code, expiresAt: expiresAt}
	pairingMu.Unlock()
	return code, expiresAt
}

// PairingStatus reports the pending code's state for the dashboard (active,
// expiry, consumed) without revealing the code. active=false once expired.
func PairingStatus() (active bool, expiresAt time.Time, used bool, deviceID string) {
	pairingMu.Lock()
	defer pairingMu.Unlock()
	if currentPairing == nil {
		return false, time.Time{}, false, ""
	}
	if !currentPairing.used && time.Now().After(currentPairing.expiresAt) {
		return false, currentPairing.expiresAt, false, ""
	}
	return true, currentPairing.expiresAt, currentPairing.used, currentPairing.deviceID
}

// staticPairingMinLen is the shortest MYCELIUM_STATIC_PAIRING_CODE accepted:
// the code never expires, so it must resist guessing on its own.
const staticPairingMinLen = 10

// StaticPairingCode returns the reusable pairing code set with
// MYCELIUM_STATIC_PAIRING_CODE (for public demo or review instances), or ""
// when unset or shorter than staticPairingMinLen.
func StaticPairingCode() string {
	c := strings.ToUpper(strings.TrimSpace(os.Getenv("MYCELIUM_STATIC_PAIRING_CODE")))
	if len(c) < staticPairingMinLen {
		return ""
	}
	return c
}

// VerifyPairingCode checks candidate against the static code, if any, then
// the pending one. A match on the pending code burns it; pairingMaxTries
// wrong guesses burn it too. The static code is never burned: guessing it is
// bounded by the per-IP pairing limiter.
func VerifyPairingCode(candidate, deviceID string) bool {
	if static := StaticPairingCode(); static != "" {
		c := strings.ToUpper(strings.TrimSpace(candidate))
		if subtle.ConstantTimeCompare([]byte(c), []byte(static)) == 1 {
			return true
		}
	}
	pairingMu.Lock()
	defer pairingMu.Unlock()
	p := currentPairing
	if p == nil || p.used || time.Now().After(p.expiresAt) {
		return false
	}
	if candidate != p.code {
		p.tries++
		if p.tries >= pairingMaxTries {
			currentPairing = nil
		}
		return false
	}
	p.used = true
	p.deviceID = deviceID
	return true
}

func randomCode(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Only fails with a broken OS entropy source.
		panic("pileus: crypto/rand unavailable: " + err.Error())
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = codeAlphabet[int(v)%len(codeAlphabet)]
	}
	return string(out)
}
