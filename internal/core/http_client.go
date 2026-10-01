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

// dnsServers are the public resolvers used for lookups.
var dnsServers = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

// dnsCacheTTL is short and fixed: CDN edge addresses rotate quickly.
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

// egressPreferIPv6 reports whether upstream connections resolve and dial
// AAAA first (A fallback) — useful when the host's IPv4 is shared (CGNAT)
// and it has a public IPv6. Only affects connections mycelium dials itself.
// Initial value from MYCELIUM_EGRESS_IPV6; the egress_ipv6 setting
// overrides it live.
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

// ParseBoolish is parseBoolish for callers outside core.
func ParseBoolish(s string) bool { return parseBoolish(s) }

// lookupHostCF resolves host with raw DNS queries to public resolvers, never
// the OS resolver. Family order follows egressPreferIPv6(); CFDialContext
// tries the other family if this one won't dial.
func lookupHostCF(ctx context.Context, host string) (string, error) {
	return lookupHostCFFamily(ctx, host, egressPreferIPv6())
}

// lookupHostCFFamily resolves host to one A or AAAA address, cached briefly
// per family (the segment proxy dials new hostnames constantly).
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

// CFDialContext dials addr resolving DNS through public resolvers directly.
func CFDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	// Preferred family first, then the other one (no AAAA, or a black-holed
	// family).
	preferV6 := egressPreferIPv6()
	var lastErr error
	for i, v6 := range [2]bool{preferV6, !preferV6} {
		ip, lerr := lookupHostCFFamily(ctx, host, v6)
		if lerr != nil {
			lastErr = lerr
			continue
		}
		// Bound the first attempt so a black-holed family falls through quickly.
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

// dockerAwareDialContext dials addr through the container engine's own DNS
// (127.0.0.11 in Docker), the only way to resolve container names or
// private/split-horizon records.
func dockerAwareDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{
		Timeout:  10 * time.Second,
		Resolver: &net.Resolver{PreferGo: true},
	}
	return d.DialContext(ctx, network, addr)
}

// httpDialContext tries the public-resolver path first and, only in Docker
// (MYCELIUM_DOCKER=1), falls back to the container engine's DNS. Outside
// Docker there is no fallback: that would be the local resolver this path
// exists to avoid.
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

// CFResolver uses a public resolver; transports use CFDialContext.
var CFResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return net.DialTimeout("udp4", "1.1.1.1:53", 5*time.Second)
	},
}

// CloudflareDialer dials through CFResolver.
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
		// net.DefaultResolver is set by cmd/server's init(); don't override it here.

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
