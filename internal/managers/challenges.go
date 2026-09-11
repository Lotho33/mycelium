package managers

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Interactive-verification tracking: some upstreams occasionally require a
// human check in a real browser before they'll serve a stream. The companion
// browser service reports this as `needs_manual_solve` (browser_client.go turns
// it into a *CobwebAPIError) and the HLS proxy sees it as a `Cf-Mitigated:
// challenge` response header. Both funnel here so the admin dashboard can list
// which (domain, egress) pairs are waiting, and the ResolveStream error handed
// to Pileus can say so plainly.

// challengeTTL: a pending entry stops being shown this long after its last hit
// — once solved it simply stops recurring, and a stale row is just noise.
const challengeTTL = 2 * time.Hour

// PendingChallenge is one (domain, egress) awaiting an interactive check.
type PendingChallenge struct {
	Domain    string    `json:"domain"`
	Egress    string    `json:"egress"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Hits      int       `json:"hits"`
}

var (
	challengeMu sync.Mutex
	challenges  = map[string]*PendingChallenge{} // key: domain + "\x00" + egress
)

func challengeKey(domain, egress string) string { return domain + "\x00" + egress }

// RecordChallenge notes that an upstream asked for interactive verification on
// domain over egress ("" / "direct" for the box's own IP, else a proxy label).
// Idempotent per (domain, egress): it bumps LastSeen + Hits.
func RecordChallenge(domain, egress string) {
	domain = strings.TrimSpace(strings.ToLower(domain))
	if domain == "" {
		return
	}
	if egress == "" {
		egress = "direct"
	}
	now := time.Now()
	challengeMu.Lock()
	defer challengeMu.Unlock()
	k := challengeKey(domain, egress)
	if c := challenges[k]; c != nil {
		c.LastSeen = now
		c.Hits++
		return
	}
	challenges[k] = &PendingChallenge{Domain: domain, Egress: egress, FirstSeen: now, LastSeen: now, Hits: 1}
}

// ClearChallenge drops a pending entry — called when a request against that
// (domain, egress) later succeeds, or from the dashboard.
func ClearChallenge(domain, egress string) {
	domain = strings.TrimSpace(strings.ToLower(domain))
	if egress == "" {
		egress = "direct"
	}
	challengeMu.Lock()
	delete(challenges, challengeKey(domain, egress))
	challengeMu.Unlock()
}

// ClearChallengeDomain drops every pending entry for a domain regardless of egress.
func ClearChallengeDomain(domain string) {
	domain = strings.TrimSpace(strings.ToLower(domain))
	challengeMu.Lock()
	for k, c := range challenges {
		if c.Domain == domain {
			delete(challenges, k)
		}
	}
	challengeMu.Unlock()
}

// PendingChallenges returns the not-yet-expired entries, most-recent first.
func PendingChallenges() []PendingChallenge {
	cut := time.Now().Add(-challengeTTL)
	challengeMu.Lock()
	out := make([]PendingChallenge, 0, len(challenges))
	for k, c := range challenges {
		if c.LastSeen.Before(cut) {
			delete(challenges, k)
			continue
		}
		out = append(out, *c)
	}
	challengeMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// ChallengePendingFor reports whether any challenge was recorded for domain
// within the last `within` — used to give ResolveStream a specific error.
func ChallengePendingFor(domain string, within time.Duration) bool {
	domain = strings.TrimSpace(strings.ToLower(domain))
	cut := time.Now().Add(-within)
	challengeMu.Lock()
	defer challengeMu.Unlock()
	for _, c := range challenges {
		if c.Domain == domain && c.LastSeen.After(cut) {
			return true
		}
	}
	return false
}
