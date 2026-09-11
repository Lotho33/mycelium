package api

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// ─── segment fetch pacing ────────────────────────────────────────────────────
// A player opens many parallel connections at stream start and asks for a
// burst of segments to fill its buffer. Some segment CDNs (vix-content.net)
// tolerate only a small burst per IP, then 403 every further request for a
// while. segAcquire caps how many upstream segment fetches run at once PER CDN
// HOST — the player's excess requests just wait here, which is invisible since
// it's buffering ahead. This is the actual fix for "first few .ts arrive, then
// a wall of 403".
var (
	segGateMu sync.Mutex
	segGates  = map[string]chan struct{}{}
)

func segConcurrency() int {
	if v := strings.TrimSpace(os.Getenv("MYCELIUM_SEGMENT_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

func segAcquire(ctx context.Context, host string) (func(), error) {
	if host == "" {
		return func() {}, nil
	}
	segGateMu.Lock()
	g := segGates[host]
	if g == nil {
		g = make(chan struct{}, segConcurrency())
		segGates[host] = g
	}
	segGateMu.Unlock()
	select {
	case g <- struct{}{}:
		return func() { <-g }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// noteChallengeIfAny records that an upstream responded with an interactive
// verification page (advertised via the `Cf-Mitigated: challenge` response
// header) so the dashboard/Pileus can tell the operator it needs solving in a
// browser. domain = registrable domain of rawURL; egressLabel is the egress
// profile name (or "direct") — a hint for the dashboard, not a key.
func noteChallengeIfAny(resp *http.Response, rawURL string, egressLabel string) {
	if resp == nil || !strings.EqualFold(resp.Header.Get("Cf-Mitigated"), "challenge") {
		return
	}
	dom := ""
	if u, err := url.Parse(rawURL); err == nil {
		dom = registrableHost(u.Hostname())
	}
	if dom == "" {
		return
	}
	if egressLabel == "" {
		egressLabel = "direct"
	}
	log.Printf("[proxy/verify] %s (egress %s) → upstream wants interactive verification", dom, egressLabel)
	managers.RecordChallenge(dom, egressLabel)
}

// videoEgressSuffix returns the query fragment ("&vpn=1&egr=<name>") that pins
// every proxied playlist/segment/key URL of pluginID to the egress profile the
// operator picked for it. Empty when the plugin's video flow goes direct.
func videoEgressSuffix(pluginID string) string {
	name := engine.LuaPlugins.PluginEgress(pluginID)
	if name == "" || name == managers.EgressDirect {
		return ""
	}
	return "&vpn=1&egr=" + url.QueryEscape(name)
}

// childVPNSuffix rebuilds the "&vpn=1[&egr=<name>]" fragment for the URLs a
// playlist rewrite emits, carrying the egress name from the incoming request.
func childVPNSuffix(q url.Values) string {
	if q.Get("vpn") != "1" {
		return ""
	}
	s := "&vpn=1"
	if egr := q.Get("egr"); egr != "" {
		s += "&egr=" + url.QueryEscape(egr)
	}
	return s
}

// challengeEgressLabel is the human hint recorded with an interactive-verification
// hit: the egress profile name when known, else "vpn"/"direct".
func challengeEgressLabel(useVPN bool, egr string) string {
	if egr != "" {
		return egr
	}
	if useVPN {
		return "vpn"
	}
	return "direct"
}

// registrableHost: last two dot labels (PSL-free) — fine for the single-label
// TLDs common among stream CDNs.
func registrableHost(host string) string {
	p := strings.Split(strings.ToLower(strings.Trim(host, ".")), ".")
	if len(p) < 2 {
		return strings.ToLower(host)
	}
	return p[len(p)-2] + "." + p[len(p)-1]
}

// segRetryBackoff is the wait before retrying a segment: a plain 403/429 is
// rate-limiting, not a transient glitch — wait seconds, not milliseconds.
func segRetryBackoff(attempt, status int) time.Duration {
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		d := time.Duration(attempt) * time.Second
		if d > 4*time.Second {
			d = 4 * time.Second
		}
		return d
	}
	return retryBackoff(attempt)
}
