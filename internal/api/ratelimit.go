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

// tokenBucket is one IP's token bucket.
type tokenBucket struct {
	tokens    float64
	lastRefil time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	rate     float64 // tokens per second
	capacity float64 // max burst
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

// allow reports whether a request from ip is allowed.
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

// cleanup drops idle buckets every 5 minutes.
func (rl *rateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		// Recover per tick with a deferred Unlock, so a panic can't wedge the
		// limiter.
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

// Per-context limiters:
//   - loginLimiter: 5 attempts/min per IP on the login form (burst 5)
//   - apiLimiter:   60 req/s per IP on the hub API (burst 20)
var (
	loginLimiter = newRateLimiter(5.0/60.0, 5)
	apiLimiter   = newRateLimiter(60, 20)
	// proxyLimiter: generous for real players, still caps an abuser.
	proxyLimiter = newRateLimiter(25, 50)
	// imgLimiter: poster grids burst on open, then go quiet.
	imgLimiter = newRateLimiter(40, 80)
	// wipeLimiter: the destructive admin actions re-check the admin password,
	// so they get the login budget, in a bucket of their own.
	wipeLimiter = newRateLimiter(5.0/60.0, 5)
)

// trustedProxyCIDRsEnv, if set, replaces trustedProxyNets with a
// comma-separated CIDR list, e.g.:
//
//	MYCELIUM_TRUSTED_PROXY_CIDRS=10.0.0.5/32
//
// for a TLS-terminating reverse proxy on a LAN address. Read once at boot.
const trustedProxyCIDRsEnv = "MYCELIUM_TRUSTED_PROXY_CIDRS"

// trustedProxyNets lists the default ranges whose X-Forwarded-For is
// trusted: loopback only (a reverse proxy on the same host). Trusting the
// LAN would let any device there fake its address and dodge per-IP limits.
var trustedProxyNets = []string{"127.0.0.0/8", "::1/128"}

// trustedProxyNetworks is the parsed list in effect (a var for tests).
var trustedProxyNetworks []*net.IPNet

func init() {
	trustedProxyNetworks = loadTrustedProxyNetworks(os.Getenv(trustedProxyCIDRsEnv))
}

// parseCIDRList parses a comma-separated CIDR list, skipping (and logging)
// invalid entries.
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

// loadTrustedProxyNetworks returns the env list when it has at least one
// valid CIDR, else trustedProxyNets.
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

// directRemoteHost is the host of r.RemoteAddr, the peer of the TCP
// connection (used by realIP and isSecureRequest).
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
	if !isTrustedProxy(remoteHost) {
		return remoteHost
	}
	// Walk from the right: each proxy appends the address it saw, so entries
	// left of the last trusted hop are whatever the client sent.
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !isTrustedProxy(hops[i]) || i == 0 {
			return hops[i]
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
