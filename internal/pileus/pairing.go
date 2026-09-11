// Device pairing via a short-lived rotating code, generated from the admin
// dashboard and typed once into Pileus. Replaces pairing with the actual
// admin password (AuthorizeDevice used to bcrypt-compare req.PinHash
// straight against master_admin_hash — see the comment removed from
// auth_handler.go): that meant every new device, including a TV reached
// only via a remote's on-screen keyboard, had to be handed the same secret
// that unlocks the whole admin panel, with no way to revoke one device's
// ability to (re-)pair without rotating that password for everything else.
// A pairing code is short (easier to type via dpad than a real password),
// expires in a few minutes, is single-use, and lives in a completely
// separate secret space — leaking one never leaks the admin password, and a
// stale/unused code left on screen goes cold on its own.
package pileus

import (
	"crypto/rand"
	"sync"
	"time"
)

// codeAlphabet excludes visually ambiguous characters (0/O, 1/I/L) — this
// code is meant to be read off a screen and typed on a TV remote.
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

// GeneratePairingCode mints a new code, replacing any still-pending one —
// only one is ever active, matching the "one manual VPN session at a time"
// pattern already used for the interactive browser session. Admin-triggered only
// (see internal/api/pileus_pairing.go); never called from Pileus itself.
func GeneratePairingCode() (code string, expiresAt time.Time) {
	code = randomCode(pairingCodeLen)
	expiresAt = time.Now().Add(pairingTTL)
	pairingMu.Lock()
	currentPairing = &pairingState{code: code, expiresAt: expiresAt}
	pairingMu.Unlock()
	return code, expiresAt
}

// PairingStatus reports the pending code's state for the dashboard to poll
// — active, expiry, and whether it's already been consumed — without ever
// echoing the code itself back out. Returns active=false once expired even
// if no one has looked at it since.
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

// VerifyPairingCode checks candidate against the pending code. Single-use:
// a correct match burns the code immediately so it can't be replayed for a
// second device. A wrong guess counts against pairingMaxTries — enough
// attempts and the code is burned too, rather than left standing for the
// rest of its TTL as a brute-forceable target on the LAN.
func VerifyPairingCode(candidate, deviceID string) bool {
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
		// crypto/rand.Read only fails if the OS entropy source is broken —
		// not a state worth degrading gracefully from.
		panic("pileus: crypto/rand unavailable: " + err.Error())
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = codeAlphabet[int(v)%len(codeAlphabet)]
	}
	return string(out)
}
