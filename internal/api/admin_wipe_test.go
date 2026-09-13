package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// wipeRoute wires a wipe handler through the exact same middleware chain
// AdminRoutes registers it with in admin.go: rate limit first, then session
// auth, then the handler itself.
func wipeRoute(handler http.HandlerFunc) http.HandlerFunc {
	return rateLimitMiddleware(wipeLimiter, adminAuthMiddleware(handler))
}

// freshWipeLimiter swaps the package-level wipeLimiter for a brand new
// instance for the duration of the test, restoring the original on cleanup.
// wipeLimiter's buckets are real-time token buckets keyed by IP — without
// this, two tests (or two `-count=N` runs of the same test) that reuse the
// same fixed test IP would share leftover budget from a previous run and
// see a 429 where the test expects 401/200, flaking under `-count=2` or a
// full `go test ./...` where these tests aren't the only ones touching this
// package's globals.
func freshWipeLimiter(t *testing.T) {
	t.Helper()
	old := wipeLimiter
	wipeLimiter = newRateLimiter(5.0/60.0, 5)
	t.Cleanup(func() { wipeLimiter = old })
}

// callWipeProfiles sends one POST /admin/profiles/wipe through the full
// chain, with a valid admin session cookie and the given password in the
// body, from remoteAddr (so tests can use distinct IPs and not share
// wipeLimiter's per-IP bucket with each other).
func callWipeProfiles(t *testing.T, sessionToken, password, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"password": password})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/profiles/wipe", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	req.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: sessionToken})
	rec := httptest.NewRecorder()
	wipeRoute(wipeProfilesHandler)(rec, req)
	return rec
}

// TestWipeProfiles_RateLimitedBeforePasswordCheck is the regression test for
// the audit finding: a stolen admin session cookie without the real password
// must not be able to brute-force it here with no limit, unlike the actual
// login form which loginLimiter already protects. It also asserts the
// ordering the fix relies on — the 429 must land on an attempt that would
// otherwise have reached (and failed) the password check, proving the rate
// limit trips independently of whatever password is guessed.
func TestWipeProfiles_RateLimitedBeforePasswordCheck(t *testing.T) {
	freshWipeLimiter(t)
	SetAdminSessionKey([]byte("test-master-secret"))
	defer SetAdminSessionKey(nil)
	seedAdminPassword(t, "correct-horse-battery")

	tok := createAdminSession()
	const remoteAddr = "203.0.113.10:5555"

	// wipeLimiter: burst 5 — five wrong-password attempts should each be
	// rejected on their own merit (401, wrong password), still within budget.
	for i := 0; i < 5; i++ {
		rec := callWipeProfiles(t, tok, "wrong-password", remoteAddr)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, body=%s; want 401 (wrong password, still under the rate limit)", i+1, rec.Code, rec.Body.String())
		}
	}

	// The 6th attempt from the same IP must be rejected by the rate limiter
	// itself — before the password is even looked at. Prove that by sending
	// the *correct* password: if the limiter didn't trip first, this would
	// otherwise succeed (200) and wipe the profiles table.
	rec := callWipeProfiles(t, tok, "correct-horse-battery", remoteAddr)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt (correct password): status = %d, body=%s; want 429 — the rate limit must trip before the password check runs", rec.Code, rec.Body.String())
	}
}

// A different IP has its own wipeLimiter bucket and is unaffected by another
// client's exhausted budget.
func TestWipeProfiles_RateLimitIsPerIP(t *testing.T) {
	freshWipeLimiter(t)
	SetAdminSessionKey([]byte("test-master-secret"))
	defer SetAdminSessionKey(nil)
	seedAdminPassword(t, "correct-horse-battery")

	tok := createAdminSession()

	for i := 0; i < 5; i++ {
		callWipeProfiles(t, tok, "wrong-password", "203.0.113.20:1")
	}
	// Exhausted the budget for .20 — confirm it is indeed exhausted.
	if rec := callWipeProfiles(t, tok, "wrong-password", "203.0.113.20:1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d; want 429 once .20's budget is exhausted", rec.Code)
	}

	// A fresh IP must still get the real (401, wrong password) response.
	rec := callWipeProfiles(t, tok, "wrong-password", "203.0.113.21:1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body=%s; want 401 — a different IP must not share .20's exhausted budget", rec.Code, rec.Body.String())
	}
}

// The rate limiter must apply even without a valid session cookie — it wraps
// adminAuthMiddleware from the outside (rateLimitMiddleware(wipeLimiter,
// auth(handler))), matching the precedent in setup.go for /proxy and /img.
func TestWipeProfiles_RateLimitAppliesBeforeAuth(t *testing.T) {
	freshWipeLimiter(t)
	const remoteAddr = "203.0.113.30:1"

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/admin/profiles/wipe", bytes.NewReader([]byte(`{}`)))
		req.RemoteAddr = remoteAddr
		req.Header.Set("Accept", "application/json") // ask adminAuthMiddleware for 401 JSON instead of a login redirect
		// No session cookie at all.
		rec := httptest.NewRecorder()
		wipeRoute(wipeProfilesHandler)(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d without a session cookie: status = %d; want 401 (auth), still under the rate limit", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/profiles/wipe", bytes.NewReader([]byte(`{}`)))
	req.RemoteAddr = remoteAddr
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	wipeRoute(wipeProfilesHandler)(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th unauthenticated attempt: status = %d; want 429", rec.Code)
	}
}
