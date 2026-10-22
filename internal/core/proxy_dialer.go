package core

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// ProxyDialer returns a dial function connecting to the target, optionally
// through the proxy in proxyURL:
//   - ""                       → direct
//   - "socks5://host:1080"     → SOCKS5 (socks5h too; user:pass@ auth)
//   - "http://host:8888"       → HTTP proxy via CONNECT
//   - "host:1080" (no scheme)  → SOCKS5
//
// With a proxy configured it never falls back to a direct connection: a
// caller that requires the proxy fails closed, and targets the proxy would
// reach on its own side (loopback, LAN, container names) are refused first
// (guardProxiedTarget). Without one the caller adds GuardedDialContext.
func ProxyDialer(proxyURL string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	if proxyURL == "" {
		return directDialWithDockerFallback(direct)
	}
	return guardProxiedTarget(proxiedDialer(proxyURL, direct))
}

func proxiedDialer(proxyURL string, direct *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	// The proxy host may be a container name, invisible to the process-wide
	// public resolver: resolve it through the container engine's DNS.
	if os.Getenv("MYCELIUM_DOCKER") == "1" {
		direct.Resolver = &net.Resolver{PreferGo: true}
	}

	// No scheme: SOCKS5.
	if !strings.Contains(proxyURL, "://") {
		proxyURL = "socks5://" + proxyURL
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("invalid proxy url %q: %w", proxyURL, err)
		}
	}

	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		var auth *xproxy.Auth
		if u.User != nil {
			pass, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: pass}
		}
		sd, serr := xproxy.SOCKS5("tcp", u.Host, auth, direct)
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			if serr != nil {
				return nil, serr
			}
			if cd, ok := sd.(xproxy.ContextDialer); ok {
				return cd.DialContext(ctx, network, addr)
			}
			return sd.Dial(network, addr)
		}
	case "http":
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			return httpConnectDial(ctx, direct, u, addr)
		}
	default:
		return func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}
}

// directDialWithDockerFallback dials through the process-wide public
// resolver and, only in Docker (MYCELIUM_DOCKER=1), retries through the
// container engine's DNS, for private/split-horizon hostnames. Outside
// Docker there is no fallback.
func directDialWithDockerFallback(direct *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := direct.DialContext(ctx, network, addr)
		if err == nil {
			return conn, nil
		}
		if os.Getenv("MYCELIUM_DOCKER") == "1" {
			fallback := &net.Dialer{
				Timeout:   direct.Timeout,
				KeepAlive: direct.KeepAlive,
				Resolver:  &net.Resolver{PreferGo: true},
			}
			if conn2, err2 := fallback.DialContext(ctx, network, addr); err2 == nil {
				return conn2, nil
			}
		}
		return nil, err
	}
}

// httpConnectTimeout bounds an HTTP proxy's CONNECT handshake.
const httpConnectTimeout = 15 * time.Second

// guardProxiedTarget refuses, before handing addr to the proxy, the targets
// that would land on the proxy's own side: IP literals that aren't public
// (loopback, link-local, private, CGNAT — through a VPN egress they can only
// mean the proxy's host or network) and names that only resolve there
// (localhost, single-label container/LAN names, .local/.internal/...). The
// name is not resolved here: the proxy does it, so the query stays off the
// local network.
func guardProxiedTarget(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if err := checkProxiedHost(host); err != nil {
			return nil, err
		}
		return dial(ctx, network, addr)
	}
}

// CheckEgressURL vets a URL handed to another process (the browser
// service) that fetches it directly (proxied=false: CheckURLNotSSRF) or
// through an egress proxy (proxied=true: checkProxiedHost).
func CheckEgressURL(ctx context.Context, rawURL string, proxied bool) error {
	if !proxied {
		return CheckURLNotSSRF(ctx, rawURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("proxy: invalid URL %q", rawURL)
	}
	return checkProxiedHost(u.Hostname())
}

// checkProxiedHost is guardProxiedTarget's test (see there).
func checkProxiedHost(host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if BlockedProxyIP(ip) || ip.IsPrivate() || cgnatNet.Contains(ip) {
			return fmt.Errorf("proxy: refusing to reach non-public address %s through an egress", ip)
		}
		return nil
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "" || !strings.Contains(h, ".") || NumericHostLike(h) {
		return fmt.Errorf("proxy: refusing to reach %q through an egress", host)
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home.arpa", ".localdomain"} {
		if strings.HasSuffix(h, suffix) {
			return fmt.Errorf("proxy: refusing to reach %q through an egress", host)
		}
	}
	return nil
}

// NumericHostLike reports whether host is an IPv4 address in one of the
// legacy inet_aton spellings net.ParseIP rejects but resolvers and proxies
// may accept ("2130706433", "0x7f.1", "017.0.0.1").
func NumericHostLike(host string) bool {
	parts := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	if len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		digits := p
		if strings.HasPrefix(p, "0x") {
			digits = p[2:]
			if digits == "" || strings.Trim(digits, "0123456789abcdef") != "" {
				return false
			}
			continue
		}
		if digits == "" || strings.Trim(digits, "0123456789") != "" {
			return false
		}
	}
	return true
}

// httpConnectDial opens a tunnel to addr through an HTTP proxy via the CONNECT
// method. It returns the raw tunneled connection, over which the caller
// performs its own TLS handshake.
func httpConnectDial(ctx context.Context, d *net.Dialer, proxyURL *url.URL, addr string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	// The CONNECT exchange is bounded even when ctx has no deadline, and a
	// cancelled ctx unblocks it.
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > httpConnectTimeout {
		deadline = time.Now().Add(httpConnectTimeout)
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	var req strings.Builder
	fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if proxyURL.User != nil {
		pass, _ := proxyURL.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + pass))
		fmt.Fprintf(&req, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	req.WriteString("\r\n")

	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		return nil, err
	}

	// The client sends its TLS ClientHello only after the CONNECT succeeds, so
	// no tunnel bytes can follow the response line yet.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Don't close resp.Body: the CONNECT response has no length, so closing it
	// would try to drain the tunnel itself and stall the handshake.
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: %s", addr, resp.Status)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
