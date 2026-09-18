package api

import (
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"
)

// tokenBucket implementa un token bucket per IP.
type tokenBucket struct {
	tokens    float64
	lastRefil time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	rate     float64 // token al secondo
	capacity float64 // burst massimo
}

func newRateLimiter(requestsPerSecond float64, burst float64) *rateLimiter {
	rl := &rateLimiter{
		buckets:  make(map[string]*tokenBucket),
		rate:     requestsPerSecond,
		capacity: burst,
	}
	go rl.cleanup()
	return rl
}

// allow ritorna true se la richiesta è consentita per quell'IP.
func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, ok := rl.buckets[ip]
	if !ok {
		b = &tokenBucket{tokens: rl.capacity, lastRefil: now}
		rl.buckets[ip] = b
	}

	elapsed := now.Sub(b.lastRefil).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.capacity {
		b.tokens = rl.capacity
	}
	b.lastRefil = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// cleanup rimuove bucket inattivi ogni 5 minuti per evitare leak di memoria.
func (rl *rateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		// This type backs loginLimiter/apiLimiter/proxyLimiter/imgLimiter — a
		// panic mid-loop without the deferred Unlock would wedge rl.mu forever
		// (every future request through this limiter blocks) instead of just
		// losing one cleanup tick.
		func() {
			defer core.Guard("api/ratelimiter-gc-tick")
			rl.mu.Lock()
			defer rl.mu.Unlock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for ip, b := range rl.buckets {
				if b.lastRefil.Before(cutoff) {
					delete(rl.buckets, ip)
				}
			}
		}()
	}
}

// Limitatori specifici per contesto:
//   - loginLimiter: max 5 tentativi/min per IP sulla pagina di login (burst 5)
//   - apiLimiter:   max 60 req/s per IP sulle API hub (burst 20)
var (
	loginLimiter = newRateLimiter(5.0/60.0, 5)
	apiLimiter   = newRateLimiter(60, 20)
	// proxyLimiter: HLS playback hammers segment/playlist URLs — a live stream
	// with 2 s parts plus a few renditions is only a handful of req/s, so 25/s
	// (burst 50) per IP is generous for real players and still caps an abuser.
	proxyLimiter = newRateLimiter(25, 50)
	// imgLimiter: poster grids burst on open, then go quiet.
	imgLimiter = newRateLimiter(40, 80)
	// wipeLimiter: /admin/profiles/wipe and /admin/plugins/wipe-data re-check
	// the admin password in the request body on top of the session cookie
	// (requireAdminPassword in admin_wipe.go) — a stolen session cookie
	// without the password is the same brute-force risk as the login form,
	// so the same budget (5 tentativi/min per IP, burst 5). A dedicated
	// bucket rather than sharing loginLimiter: a legitimate admin fumbling
	// the wipe confirmation shouldn't burn through the budget they need to
	// log back in, and vice versa.
	wipeLimiter = newRateLimiter(5.0/60.0, 5)
)

// trustedProxyCIDRsEnv, if set, REPLACES trustedProxyNets entirely with the
// operator-supplied CIDR list (comma-separated), e.g.:
//
//	MYCELIUM_TRUSTED_PROXY_CIDRS=10.0.0.5/32
//	MYCELIUM_TRUSTED_PROXY_CIDRS=10.0.0.5/32,192.168.1.10/32
//
// Use this when the real reverse proxy terminating TLS runs on a dedicated
// LAN IP rather than on loopback. It is read once at boot (see init below).
const trustedProxyCIDRsEnv = "MYCELIUM_TRUSTED_PROXY_CIDRS"

// trustedProxyNets lists the default CIDR ranges whose X-Forwarded-For
// header is trusted when the direct connection comes from them.
//
// Only loopback is trusted by default. A TLS-terminating reverse proxy
// typically talks to mycelium over 127.0.0.1/::1 (same host, same network
// namespace, or Docker's loopback-equivalent between a proxy and app
// container sharing a pod/network). Trusting the *entire* RFC1918 space (the
// previous default) meant any device on a home LAN, or any other container
// on the same Docker bridge network, satisfied "trusted proxy" for its own
// direct connection — letting it set an arbitrary X-Forwarded-For on every
// request and dodge per-IP rate limiting (e.g. brute-forcing /admin/login,
// or resetting the /proxy and /img limiters). An operator who genuinely
// fronts mycelium with a reverse proxy on a dedicated non-loopback LAN IP
// must opt in explicitly via MYCELIUM_TRUSTED_PROXY_CIDRS (see above) —
// this is a breaking change from earlier versions, intentionally so.
var trustedProxyNets = []string{"127.0.0.0/8", "::1/128"}

// trustedProxyNetworks is the parsed form of the trusted-proxy CIDR list
// actually in effect — either trustedProxyNets or the MYCELIUM_TRUSTED_PROXY_CIDRS
// override. Built once at init via loadTrustedProxyNetworks; kept as a var
// (not computed inline) so tests can rebuild it after changing the env var.
var trustedProxyNetworks []*net.IPNet

func init() {
	trustedProxyNetworks = loadTrustedProxyNetworks(os.Getenv(trustedProxyCIDRsEnv))
}

// parseCIDRList parses a comma-separated CIDR list. Any entry that fails to
// parse is skipped (with a warning logged) rather than aborting the whole
// list — a single typo shouldn't also cost the other, valid entries. Pure
// function (no env/global access) so it's directly testable.
func parseCIDRList(raw string) []*net.IPNet {
	var nets []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			log.Printf("[ratelimit] %s: CIDR non valida %q, ignorata: %v", trustedProxyCIDRsEnv, part, err)
			continue
		}
		nets = append(nets, network)
	}
	return nets
}

// loadTrustedProxyNetworks builds the effective trusted-proxy network list
// from the given env var value: if it contains at least one valid CIDR,
// that list REPLACES the default entirely. Otherwise (env unset, empty, or
// every entry malformed) it falls back to trustedProxyNets (loopback only).
// Exposed as a standalone pure function (rather than inlined in init) so
// tests can rebuild trustedProxyNetworks after changing the env var —
// init() itself only runs once per process.
func loadTrustedProxyNetworks(envVal string) []*net.IPNet {
	if envVal = strings.TrimSpace(envVal); envVal != "" {
		if nets := parseCIDRList(envVal); len(nets) > 0 {
			return nets
		}
		log.Printf("[ratelimit] %s impostata ma nessuna CIDR valida trovata, uso il default (%v)", trustedProxyCIDRsEnv, trustedProxyNets)
	}
	var nets []*net.IPNet
	for _, cidr := range trustedProxyNets {
		if _, network, err := net.ParseCIDR(cidr); err == nil {
			nets = append(nets, network)
		}
	}
	return nets
}

func isTrustedProxy(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, network := range trustedProxyNetworks {
		if network.Contains(parsed) {
			return true
		}
	}
	return false
}

// directRemoteHost extracts the host part of r.RemoteAddr — the peer that
// actually opened the TCP connection, before any Forwarded-* header is
// considered. Shared by realIP (X-Forwarded-For trust) and isSecureRequest
// (X-Forwarded-Proto trust in admin_auth_pages.go) so both apply the same
// "trusted proxy" notion of who is allowed to speak for the original client.
func directRemoteHost(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}
	return remoteHost
}

func realIP(r *http.Request) string {
	remoteHost := directRemoteHost(r)
	// Only honour X-Forwarded-For when the direct connection comes from a trusted proxy.
	if isTrustedProxy(remoteHost) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if first = strings.TrimSpace(first); first != "" {
				return first
			}
		}
	}
	return remoteHost
}

func rateLimitMiddleware(limiter *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow(realIP(r)) {
			http.Error(w, `{"detail":"troppe richieste, riprova tra poco"}`, http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
