package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// dnsServers is the ordered list of DNS servers used for all lookups.
var dnsServers = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

// dnsCacheTTL is deliberately short and fixed rather than parsed from the DNS
// response's real TTL: CDN edge IPs (the dominant caller here, via the HLS
// proxy hitting a new hostname almost every segment) can rotate quickly, and
// a short fixed TTL bounds staleness without needing to parse RR TTLs at all.
const dnsCacheTTL = 45 * time.Second

type dnsCacheEntry struct {
	ip      string
	expires time.Time
}

var dnsCache sync.Map // "host|4" / "host|6" -> dnsCacheEntry

const (
	dnsTypeA    uint16 = 1
	dnsTypeAAAA uint16 = 28
)

// egressPreferIPv6 reports whether upstream CDN fetches should resolve+dial
// AAAA first (with A fallback). Useful where the box's IPv4 egress is CGNAT'd
// / on a shared address range with a poor reputation but it has a clean
// public IPv6 (a common home-server / Proxmox-CT situation): some CDNs reject
// requests from the shared IPv4 while the same request over the dedicated
// IPv6 goes through. Only affects connections mycelium dials itself (the
// HLS relay through the browser sidecar resolves on its own).
//
// Initial value from MYCELIUM_EGRESS_IPV6; the dashboard setting
// (egress_ipv6, applied via SetEgressPreferIPv6) overrides it live.
func egressPreferIPv6() bool { return preferIPv6.Load() }

var preferIPv6 = func() *atomic.Bool {
	b := new(atomic.Bool)
	b.Store(parseBoolish(os.Getenv("MYCELIUM_EGRESS_IPV6")))
	return b
}()

// SetEgressPreferIPv6 switches the dial family preference at runtime.
func SetEgressPreferIPv6(v bool) { preferIPv6.Store(v) }

// EgressPreferIPv6 reports the current dial family preference.
func EgressPreferIPv6() bool { return preferIPv6.Load() }

// parseBoolish reads "1"/"true"/"yes"/"on" (any case) as true.
func parseBoolish(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ParseBoolish is parseBoolish for callers outside core (settings values).
func ParseBoolish(s string) bool { return parseBoolish(s) }

// lookupHostCF resolves host via raw DNS UDP queries to Cloudflare/Google/Quad9.
// It never falls back to the OS resolver, so Starlink IPv6 DNS is never consulted.
// Family order follows egressPreferIPv6(); the caller (CFDialContext) still
// tries the other family if this one won't dial.
func lookupHostCF(ctx context.Context, host string) (string, error) {
	return lookupHostCFFamily(ctx, host, egressPreferIPv6())
}

// lookupHostCFFamily resolves host to a single A (v6=false) or AAAA (v6=true)
// address. Results are cached briefly per family: without this, every dial to a
// new hostname (the common case for the HLS segment proxy) paid a full DNS
// round-trip on top of the TCP+TLS handshake, on the request's critical path.
func lookupHostCFFamily(ctx context.Context, host string, v6 bool) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}

	cacheKey := host + "|4"
	qtype := dnsTypeA
	if v6 {
		cacheKey = host + "|6"
		qtype = dnsTypeAAAA
	}

	if v, ok := dnsCache.Load(cacheKey); ok {
		entry := v.(dnsCacheEntry)
		if time.Now().Before(entry.expires) {
			return entry.ip, nil
		}
		dnsCache.Delete(cacheKey)
	}

	fqdn := host
	if !strings.HasSuffix(fqdn, ".") {
		fqdn = fqdn + "."
	}

	qid := uint16(rand.Uint32())
	query := buildDNSQuery(qid, fqdn, qtype)

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(8 * time.Second)
	}

	for _, server := range dnsServers {
		conn, err := net.DialTimeout("udp4", server, 3*time.Second)
		if err != nil {
			continue
		}
		conn.SetDeadline(deadline)
		if _, err = conn.Write(query); err != nil {
			conn.Close()
			continue
		}
		buf := make([]byte, 1500)
		n, err := conn.Read(buf)
		conn.Close()
		if err != nil {
			continue
		}
		ip, err := parseDNSResponse(buf[:n], qid)
		if err != nil {
			continue
		}
		dnsCache.Store(cacheKey, dnsCacheEntry{ip: ip, expires: time.Now().Add(dnsCacheTTL)})
		return ip, nil
	}
	return "", fmt.Errorf("DNS lookup %s (%s): all servers failed", strings.TrimSuffix(fqdn, "."), map[bool]string{true: "AAAA", false: "A"}[v6])
}

// buildDNSQuery builds a minimal DNS query packet for qtype (A or AAAA).
func buildDNSQuery(id uint16, fqdn string, qtype uint16) []byte {
	buf := make([]byte, 0, 512)
	// Header
	buf = append(buf, byte(id>>8), byte(id))
	buf = append(buf, 0x01, 0x00) // QR=0 OPCODE=0 RD=1
	buf = append(buf, 0x00, 0x01) // QDCOUNT=1
	buf = append(buf, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	// Question: encode FQDN labels
	for _, label := range strings.Split(strings.TrimSuffix(fqdn, "."), ".") {
		buf = append(buf, byte(len(label)))
		buf = append(buf, []byte(label)...)
	}
	buf = append(buf, 0x00)                        // root label
	buf = append(buf, byte(qtype>>8), byte(qtype)) // QTYPE (A=1 / AAAA=28)
	buf = append(buf, 0x00, 0x01)                  // QCLASS=IN
	return buf
}

// parseDNSResponse extracts the first A or AAAA record IP from a DNS response.
func parseDNSResponse(buf []byte, expectedID uint16) (string, error) {
	if len(buf) < 12 {
		return "", fmt.Errorf("response too short")
	}
	if binary.BigEndian.Uint16(buf[0:2]) != expectedID {
		return "", fmt.Errorf("ID mismatch")
	}
	rcode := buf[3] & 0x0F
	if rcode != 0 {
		return "", fmt.Errorf("RCODE=%d (NXDOMAIN or error)", rcode)
	}
	ancount := binary.BigEndian.Uint16(buf[6:8])
	if ancount == 0 {
		return "", fmt.Errorf("no answers")
	}

	// Skip question section
	pos := 12
	qdcount := binary.BigEndian.Uint16(buf[4:6])
	for i := 0; i < int(qdcount); i++ {
		for pos < len(buf) {
			l := int(buf[pos])
			pos++
			if l == 0 {
				break
			}
			if l&0xC0 == 0xC0 {
				pos++
				break
			}
			pos += l
		}
		pos += 4 // QTYPE + QCLASS
	}

	// Parse answer section
	for i := 0; i < int(ancount); i++ {
		if pos >= len(buf) {
			break
		}
		// Skip name (may be pointer)
		for pos < len(buf) {
			l := int(buf[pos])
			if l&0xC0 == 0xC0 {
				pos += 2
				break
			}
			pos++
			if l == 0 {
				break
			}
			pos += l
		}
		if pos+10 > len(buf) {
			break
		}
		rtype := binary.BigEndian.Uint16(buf[pos : pos+2])
		rdlen := int(binary.BigEndian.Uint16(buf[pos+8 : pos+10]))
		pos += 10
		if rtype == dnsTypeA && rdlen == 4 && pos+4 <= len(buf) {
			return fmt.Sprintf("%d.%d.%d.%d", buf[pos], buf[pos+1], buf[pos+2], buf[pos+3]), nil
		}
		if rtype == dnsTypeAAAA && rdlen == 16 && pos+16 <= len(buf) {
			return net.IP(buf[pos : pos+16]).String(), nil
		}
		pos += rdlen
	}
	return "", fmt.Errorf("no A/AAAA record in response")
}

// CFDialContext dials addr resolving DNS via Cloudflare/Google UDP directly —
// never touching the OS/Starlink resolver.
func CFDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	// Try the preferred family first, then the other one — so a box that
	// prefers IPv6 (MYCELIUM_EGRESS_IPV6) still works if a given CDN has no
	// AAAA or IPv6 is black-holed, and vice-versa.
	preferV6 := egressPreferIPv6()
	var lastErr error
	for i, v6 := range [2]bool{preferV6, !preferV6} {
		ip, lerr := lookupHostCFFamily(ctx, host, v6)
		if lerr != nil {
			lastErr = lerr
			continue
		}
		// Bound the first attempt so a black-holed family falls through
		// quickly; give the second attempt the remaining budget.
		attemptCtx := ctx
		if i == 0 {
			var cancel context.CancelFunc
			attemptCtx, cancel = context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
		}
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		conn, derr := d.DialContext(attemptCtx, network, net.JoinHostPort(ip, port))
		fam := "v4"
		if v6 {
			fam = "v6"
		}
		if derr == nil {
			log.Printf("[core/dial] %s → %s (%s)", host, ip, fam)
			return conn, nil
		}
		log.Printf("[core/dial] %s %s failed: %v", host, fam, derr)
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("CFDialContext %s: no address", host)
	}
	return nil, lastErr
}

// dockerAwareDialContext dials addr using the container engine's own DNS
// (127.0.0.11 for Docker, forwarding to whatever the host/daemon has
// configured) instead of a raw public resolver — the only way to ever
// resolve a Docker-internal name or a user's private/split-horizon DNS
// record (e.g. a self-hosted Jellyfin/Plex reachable only via a local
// hostname that Cloudflare/Google/Quad9 have never heard of). Same 3-line
// pattern already used in managers.InitRedis/core.ProxyDialer, duplicated
// here rather than shared — this codebase keeps it local per package instead
// of a cross-package helper for something this small.
func dockerAwareDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout:  10 * time.Second,
		Resolver: &net.Resolver{PreferGo: true},
	}
	return d.DialContext(ctx, network, addr)
}

// httpDialContext tries the direct-to-public-resolver path first (protects
// every plugin against a broken/hijacking ISP resolver on public domains,
// the original reason CFDialContext exists) and only falls back to the
// container engine's own DNS — and only when MYCELIUM_DOCKER=1 — if that
// fails. Outside Docker there is no such fallback: the "OS resolver" there
// would just be the same possibly-hijacked ISP resolver CFDialContext exists
// to avoid, so falling back to it would silently reintroduce that bug.
func httpDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := CFDialContext(ctx, network, addr)
	if err == nil {
		return conn, nil
	}
	if os.Getenv("MYCELIUM_DOCKER") == "1" {
		if conn2, err2 := dockerAwareDialContext(ctx, network, addr); err2 == nil {
			return conn2, nil
		}
	}
	return nil, err
}

// CFResolver is kept for compatibility — prefer CFDialContext for transports.
var CFResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return net.DialTimeout("udp4", "1.1.1.1:53", 5*time.Second)
	},
}

// CloudflareDialer kept for compatibility.
var CloudflareDialer = &net.Dialer{
	Timeout:   30 * time.Second,
	KeepAlive: 30 * time.Second,
	Resolver:  CFResolver,
}

var (
	HTTPClient     *http.Client
	httpClientOnce sync.Once
)

func SetupHTTPClient() {
	httpClientOnce.Do(func() {
		// net.DefaultResolver non va toccato qui: cmd/server/main.go init()
		// lo imposta già (con fallback multi-server 1.1.1.1→8.8.8.8 e rispetto
		// del deadline del context) prima che main() chiami questa funzione.
		// Riassegnarlo a CFResolver lo sostituiva con una versione più debole
		// (un solo server, timeout fisso a 5s, nessun fallback) — le due
		// implementazioni esistono per motivi storici distinti, ma solo una
		// deve vincere per net.DefaultResolver.

		t := &http.Transport{
			DialContext:         httpDialContext,
			MaxIdleConns:        100,
			MaxConnsPerHost:     100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		}
		HTTPClient = &http.Client{
			Timeout:   20 * time.Second,
			Transport: t,
		}
		log.Println("🌐 Client HTTP con DNS Cloudflare 1.1.1.1 inizializzato.")
	})
}
