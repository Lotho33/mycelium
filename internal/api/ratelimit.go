package api

import (
	"net"
	"net/http"
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
)

// trustedProxyNets lists CIDR ranges whose X-Forwarded-For header is trusted.
// Only loopback and RFC-1918 ranges are trusted by default (reverse-proxy on same host/LAN).
var trustedProxyNets = []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// trustedProxyNetworks is the pre-parsed form of trustedProxyNets, built once at init.
var trustedProxyNetworks []*net.IPNet

func init() {
	for _, cidr := range trustedProxyNets {
		if _, network, err := net.ParseCIDR(cidr); err == nil {
			trustedProxyNetworks = append(trustedProxyNetworks, network)
		}
	}
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

func realIP(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}
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
