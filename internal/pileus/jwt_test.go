package pileus

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testAuthHandler() *AuthHandler {
	return NewAuthHandler([]byte("test-secret-do-not-use-in-prod-1"))
}

func TestJWT_RoundTrip(t *testing.T) {
	h := testAuthHandler()
	tok, err := h.mintJWT("device-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	id, err := h.ParseJWT(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if id != "device-1" {
		t.Fatalf("ParseJWT device = %q, want device-1", id)
	}
}

func TestJWT_ExpiredRejected(t *testing.T) {
	h := testAuthHandler()
	tok, err := h.mintJWT("device-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := h.ParseJWT(tok); err == nil {
		t.Fatal("expired token was accepted")
	}
}

func TestJWT_WrongSecretRejected(t *testing.T) {
	h1 := NewAuthHandler([]byte("secret-one-do-not-use-in-prod-a"))
	h2 := NewAuthHandler([]byte("secret-two-do-not-use-in-prod-b"))
	tok, err := h1.mintJWT("device-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := h2.ParseJWT(tok); err == nil {
		t.Fatal("token verified against the wrong secret")
	}
}

func TestJWT_TamperedSignatureRejected(t *testing.T) {
	h := testAuthHandler()
	tok, err := h.mintJWT("device-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Flip a base64url char in the signature — but NOT its last char: base64's
	// trailing char in a non-multiple-of-3 group carries a couple of "don't
	// care" padding bits, so changing only that one can decode to the exact
	// same bytes and leave the signature verifying fine (flaky test, not a
	// real bug — found while re-running the suite for an unrelated change).
	// Any earlier char always sits in a full 3-byte group, so it always flips
	// real signature bytes.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || len(parts[2]) < 2 {
		t.Fatalf("unexpected JWT shape: %q", tok)
	}
	sig := []byte(parts[2])
	flip := byte('A')
	if sig[0] == 'A' {
		flip = 'B'
	}
	sig[0] = flip
	tampered := parts[0] + "." + parts[1] + "." + string(sig)
	if _, err := h.ParseJWT(tampered); err == nil {
		t.Fatal("tampered signature was accepted")
	}
}

func TestJWT_MalformedRejectedNotPanics(t *testing.T) {
	h := testAuthHandler()
	for _, bad := range []string{"", "not-a-jwt", "a.b.c", "Bearer abc"} {
		if _, err := h.ParseJWT(bad); err == nil {
			t.Errorf("ParseJWT(%q) accepted, want an error", bad)
		}
	}
}

// alg confusion: the keyfunc must refuse any signing method that isn't HMAC,
// no matter how well-formed the rest of the token is — accepting "none" or an
// asymmetric algorithm here would let an attacker forge a token for any
// device (see server.go's authInterceptor, the only thing standing between an
// unauthenticated caller and every Pileus RPC).
func TestJWT_AlgNoneRejected(t *testing.T) {
	h := testAuthHandler()
	claims := pileusJWTClaims{
		DeviceID: "device-1",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign alg=none: %v", err)
	}
	if _, err := h.ParseJWT(signed); err == nil {
		t.Fatal("alg=none token was accepted")
	}
}

func TestJWT_AsymmetricAlgRejected(t *testing.T) {
	h := testAuthHandler()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	claims := pileusJWTClaims{
		DeviceID: "device-1",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign RS256: %v", err)
	}
	if _, err := h.ParseJWT(signed); err == nil {
		t.Fatal("RS256-signed token was accepted — alg-confusion guard broken")
	}
}
