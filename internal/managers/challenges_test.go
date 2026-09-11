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
