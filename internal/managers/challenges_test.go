package managers

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestChallengeRegistry(t *testing.T) {
	// isolate
	challengeMu.Lock()
	challenges = map[string]*PendingChallenge{}
	challengeMu.Unlock()

	RecordChallenge("Cdn.Example", "vpn")
	RecordChallenge("cdn.example", "vpn") // same (domain,egress) → bump, not a new row
	RecordChallenge("other.example", "")

	got := PendingChallenges()
	if len(got) != 2 {
		t.Fatalf("want 2 pending, got %d: %+v", len(got), got)
	}
	var row *PendingChallenge
	for i := range got {
		if got[i].Domain == "cdn.example" {
			row = &got[i]
		}
	}
	if row == nil || row.Hits != 2 || row.Egress != "vpn" {
		t.Fatalf("cdn.example row wrong: %+v", row)
	}
	if !ChallengePendingFor("cdn.example", time.Minute) {
		t.Errorf("ChallengePendingFor should see the fresh hit")
	}

	ClearChallengeDomain("cdn.example")
	if ChallengePendingFor("cdn.example", time.Hour) {
		t.Errorf("cleared domain still pending")
	}
	if n := len(PendingChallenges()); n != 1 {
		t.Errorf("want 1 left after clear, got %d", n)
	}
}

// TestPruneExpiredChallenges verifies the GC logic the background ticker
// runs (see init() in challenges.go) removes stale entries by itself,
// independently of PendingChallenges ever being called (e.g. the admin
// dashboard is never opened).
func TestPruneExpiredChallenges(t *testing.T) {
	challengeMu.Lock()
	challenges = map[string]*PendingChallenge{}
	challengeMu.Unlock()

	RecordChallenge("fresh.example", "vpn")

	// Backdate an entry past challengeTTL without going through
	// PendingChallenges, so this exercises the shared pruning function
	// directly rather than its on-demand caller.
	expiredKey := challengeKey("expired.example", "direct")
	challengeMu.Lock()
	challenges[expiredKey] = &PendingChallenge{
		Domain:    "expired.example",
		Egress:    "direct",
		FirstSeen: time.Now().Add(-3 * challengeTTL),
		LastSeen:  time.Now().Add(-challengeTTL - time.Minute),
		Hits:      1,
	}
	challengeMu.Unlock()

	pruneExpiredChallenges()

	challengeMu.Lock()
	_, expiredStillThere := challenges[expiredKey]
	_, freshStillThere := challenges[challengeKey("fresh.example", "vpn")]
	remaining := len(challenges)
	challengeMu.Unlock()

	if expiredStillThere {
		t.Errorf("pruneExpiredChallenges left an expired entry in place")
	}
	if !freshStillThere {
		t.Errorf("pruneExpiredChallenges removed a fresh entry it shouldn't have touched")
	}
	if remaining != 1 {
		t.Errorf("want 1 entry left after pruning, got %d", remaining)
	}
}

func TestChallengeDomain_UnwrapsCobwebError(t *testing.T) {
	wrapped := fmt.Errorf("resolve: %w", &CobwebAPIError{Status: 422, Kind: "needs_manual_solve", Domain: "cdn.example"})
	dom, ok := ChallengeDomain(wrapped)
	if !ok || dom != "cdn.example" {
		t.Fatalf("ChallengeDomain = (%q, %v), want (cdn.example, true)", dom, ok)
	}
	if _, ok := ChallengeDomain(errors.New("plain")); ok {
		t.Errorf("a plain error must not read as a challenge")
	}
	if _, ok := ChallengeDomain(fmt.Errorf("x: %w", &CobwebAPIError{Status: 500, Kind: "internal"})); ok {
		t.Errorf("a non-challenge CobwebAPIError must not read as a challenge")
	}
}
