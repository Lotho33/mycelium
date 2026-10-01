package pileus

import (
	"testing"
	"time"
)

func TestPairingHappyPath(t *testing.T) {
	code, expiresAt := GeneratePairingCode()
	if len(code) != pairingCodeLen {
		t.Fatalf("code length = %d, want %d", len(code), pairingCodeLen)
	}
	if !expiresAt.After(time.Now()) {
		t.Fatalf("expiresAt not in the future")
	}
	active, _, used, _ := PairingStatus()
	if !active || used {
		t.Fatalf("status after generate: active=%v used=%v, want active=true used=false", active, used)
	}

	if !VerifyPairingCode(code, "device-1") {
		t.Fatalf("VerifyPairingCode: correct code rejected")
	}
	active, _, used, deviceID := PairingStatus()
	if !active || !used || deviceID != "device-1" {
		t.Fatalf("status after use: active=%v used=%v deviceID=%q", active, used, deviceID)
	}

	// Single-use: the same code must not verify a second device.
	if VerifyPairingCode(code, "device-2") {
		t.Fatalf("VerifyPairingCode: consumed code accepted a second time")
	}
}

func TestPairingWrongCodeRejected(t *testing.T) {
	code, _ := GeneratePairingCode()
	if VerifyPairingCode(code+"X", "device-1") {
		t.Fatalf("VerifyPairingCode: wrong code accepted")
	}
}

func TestPairingLockoutAfterMaxTries(t *testing.T) {
	code, _ := GeneratePairingCode()
	for i := 0; i < pairingMaxTries; i++ {
		if VerifyPairingCode("WRONG!", "device-1") {
			t.Fatalf("VerifyPairingCode: garbage guess unexpectedly accepted")
		}
	}
	// The code should now be burned even though it was never guessed
	// correctly — otherwise it's a standing target for the rest of its TTL.
	if VerifyPairingCode(code, "device-1") {
		t.Fatalf("VerifyPairingCode: correct code still accepted after lockout")
	}
	active, _, _, _ := PairingStatus()
	if active {
		t.Fatalf("status still active after lockout")
	}
}

func TestPairingExpiry(t *testing.T) {
	code, _ := GeneratePairingCode()
	pairingMu.Lock()
	currentPairing.expiresAt = time.Now().Add(-time.Second)
	pairingMu.Unlock()

	active, _, _, _ := PairingStatus()
	if active {
		t.Fatalf("status still active past expiry")
	}
	if VerifyPairingCode(code, "device-1") {
		t.Fatalf("VerifyPairingCode: expired code accepted")
	}
}

func TestPairingNoneActive(t *testing.T) {
	pairingMu.Lock()
	currentPairing = nil
	pairingMu.Unlock()

	active, _, used, _ := PairingStatus()
	if active || used {
		t.Fatalf("status with no pending code: active=%v used=%v, want both false", active, used)
	}
	if VerifyPairingCode("ANYCODE", "device-1") {
		t.Fatalf("VerifyPairingCode: accepted with no pending code")
	}
}

func TestStaticPairingCode(t *testing.T) {
	t.Setenv("MYCELIUM_STATIC_PAIRING_CODE", "demo-Review-2026")
	for i, dev := range []string{"device-1", "device-2"} {
		if !VerifyPairingCode(" DEMO-review-2026 ", dev) {
			t.Fatalf("static code rejected on use %d (case/space-insensitive, reusable)", i+1)
		}
	}
	if VerifyPairingCode("DEMO-REVIEW-2025", "device-3") {
		t.Fatalf("wrong static code accepted")
	}

	// The rotating code keeps working next to the static one.
	code, _ := GeneratePairingCode()
	if !VerifyPairingCode(code, "device-4") {
		t.Fatalf("rotating code rejected while a static code is set")
	}
}

func TestStaticPairingCodeTooShort(t *testing.T) {
	t.Setenv("MYCELIUM_STATIC_PAIRING_CODE", "SHORT1234")
	if StaticPairingCode() != "" {
		t.Fatalf("code shorter than %d chars must be ignored", staticPairingMinLen)
	}
	pairingMu.Lock()
	currentPairing = nil
	pairingMu.Unlock()
	if VerifyPairingCode("SHORT1234", "device-1") {
		t.Fatalf("too-short static code accepted")
	}
}
