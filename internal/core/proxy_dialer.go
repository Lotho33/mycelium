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
// caller that requires the proxy fails closed.
func ProxyDialer(proxyURL string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	if proxyURL == "" {
		return directDialWithDockerFallback(direct)
	}

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
	case "http", "https":
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

// httpConnectDial opens a tunnel to addr through an HTTP proxy via the CONNECT
// method. It returns the raw tunneled connection, over which the caller
// performs its own TLS handshake.
func httpConnectDial(ctx context.Context, d *net.Dialer, proxyURL *url.URL, addr string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

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
