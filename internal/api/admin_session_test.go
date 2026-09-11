package api

import (
	"strconv"
	"testing"
	"time"
)

func TestAdminSession_RoundTrip(t *testing.T) {
	SetAdminSessionKey([]byte("test-master-secret"))
	defer SetAdminSessionKey(nil)

	tok := createAdminSession()
	if !isValidAdminSession(tok) {
		t.Fatal("freshly minted session rejected")
	}
}

// The whole point of P2-6: nothing server-side needs to remember a session to
// validate it, so a process restart (which wipes every in-memory map) must
// not invalidate an unexpired one.
func TestAdminSession_SurvivesSimulatedRestart(t *testing.T) {
	SetAdminSessionKey([]byte("test-master-secret"))
	defer SetAdminSessionKey(nil)

	tok := createAdminSession()

	// Simulate a restart: re-derive the same key from the same master (as
	// main.go does on every boot) and wipe whatever in-memory state exists —
	// there is none left for a valid session to depend on.
	revokedAdminSessionsMu.Lock()
	revokedAdminSessions = make(map[string]int64)
	revokedAdminSessionsMu.Unlock()
	SetAdminSessionKey([]byte("test-master-secret"))

	if !isValidAdminSession(tok) {
		t.Fatal("session invalidated by a simulated restart — the bug this fix closes")
	}
}

func TestAdminSession_ExpiredRejected(t *testing.T) {
	SetAdminSessionKey([]byte("test-master-secret"))
	defer SetAdminSessionKey(nil)

	id := generateSessionToken()
	exp := time.Now().Add(-time.Hour).Unix() // already expired
	tok := id + "." + strconv.FormatInt(exp, 10) + "." + signAdminSession(id, exp)
	if isValidAdminSession(tok) {
		t.Fatal("expired session accepted")
	}
}

func TestAdminSession_TamperedRejected(t *testing.T) {
	SetAdminSessionKey([]byte("test-master-secret"))
	defer SetAdminSessionKey(nil)

	tok := createAdminSession()
	// Corrupt the signature's last hex digit. Unlike base64 (see the JWT
	// tamper test), hex has no partial-group "don't care" bits — any digit
	// change here always changes the decoded byte, so a fixed replacement
	// char is fine as long as it actually differs from the original.
	last := tok[len(tok)-1]
	flip := byte('0')
	if last == '0' {
		flip = '1'
	}
	tampered := tok[:len(tok)-1] + string(flip)
	if isValidAdminSession(tampered) {
		t.Fatal("tampered session accepted")
	}
}

func TestAdminSession_WrongKeyRejected(t *testing.T) {
	SetAdminSessionKey([]byte("secret-one"))
	tok := createAdminSession()
	SetAdminSessionKey([]byte("secret-two"))
	defer SetAdminSessionKey(nil)

	if isValidAdminSession(tok) {
		t.Fatal("session signed with a different key was accepted")
	}
}

func TestAdminSession_MalformedRejectedNotPanics(t *testing.T) {
	SetAdminSessionKey([]byte("test-master-secret"))
	defer SetAdminSessionKey(nil)

	for _, bad := range []string{"", "not-a-session", "a.b", "a.b.c.d"} {
		if isValidAdminSession(bad) {
			t.Errorf("isValidAdminSession(%q) accepted, want rejected", bad)
		}
	}
}

func TestAdminSession_NoKeyConfiguredFailsClosed(t *testing.T) {
	SetAdminSessionKey([]byte("k"))
	tok := createAdminSession()
	SetAdminSessionKey(nil)
	if isValidAdminSession(tok) {
		t.Fatal("session accepted with no signing key configured")
	}
}

// Explicit logout still works immediately (it's the one thing that does need
// server-side state — see the doc comment on the session vars).
func TestAdminSession_RevokeInvalidatesImmediately(t *testing.T) {
	SetAdminSessionKey([]byte("test-master-secret"))
	defer SetAdminSessionKey(nil)

	tok := createAdminSession()
	if !isValidAdminSession(tok) {
		t.Fatal("session should be valid before revocation")
	}
	revokeAdminSession(tok)
	if isValidAdminSession(tok) {
		t.Fatal("revoked session still validates")
	}
}
