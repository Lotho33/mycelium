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

// ProxyDialer returns a dial function that establishes a raw TCP connection to
// the target address, optionally tunneled through the proxy given by proxyURL.
//
//   - ""                       → direct connection (plain dialer)
//   - "socks5://host:1080"     → SOCKS5 (optionally socks5h; auth via user:pass@;
//     e.g. the WARP sidecar's socks5://warp:1080)
//   - "http://host:8888"       → HTTP proxy via CONNECT
//   - "host:1080" (no scheme)  → treated as SOCKS5 (legacy bare-host config)
//
// The returned dialer never falls back to a direct connection when a proxy is
// configured: if the proxy is misconfigured or unreachable the dial fails, so a
// caller that requires the proxy fails closed rather than leaking its real IP.
func ProxyDialer(proxyURL string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	direct := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	if proxyURL == "" {
		return directDialWithDockerFallback(direct)
	}

	// The process-wide net.DefaultResolver is overridden (see cmd/server/main.go
	// init()) to force all lookups through Cloudflare 1.1.1.1, which cannot
	// resolve container-internal hostnames. proxyURL's host (e.g. "warp") is
	// exactly that kind of hostname, so it needs the container engine's own
	// resolver (127.0.0.11 for Docker) instead — same fix as managers.InitRedis.
	if os.Getenv("MYCELIUM_DOCKER") == "1" {
		direct.Resolver = &net.Resolver{PreferGo: true}
	}

	// Legacy back-compat: "127.0.0.1:40000" without scheme means SOCKS5.
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

// directDialWithDockerFallback tries the process-wide resolver first
// (net.DefaultResolver, hijacked to Cloudflare-only by cmd/server/main.go
// init() — protects every plugin against a broken/hijacking ISP resolver on
// public domains, which is why that override exists) and, only when
// MYCELIUM_DOCKER=1, retries via the container engine's own DNS (127.0.0.11
// for Docker) if that fails. Unlike the proxy-host case below, a plain
// direct dial's target can legitimately be either a public scraping domain
// (where the Cloudflare-first path is exactly the protection we want) or a
// user's private/split-horizon hostname (e.g. a self-hosted Jellyfin/Plex
// server) that no public resolver has ever heard of — a fallback instead of
// an unconditional switch keeps both working. Outside Docker there is no
// fallback: the OS resolver there would just be the same possibly-hijacked
// ISP resolver this whole mechanism exists to avoid.
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

	// The client sends the TLS ClientHello only after this CONNECT succeeds, so
	// the proxy cannot have pipelined bytes past the response line yet — a bufio
	// reader will not swallow tunnel data here.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Do NOT call resp.Body.Close(): the proxy's CONNECT response has no
	// Content-Length/Transfer-Encoding, so Go treats its body as
	// read-until-EOF. Closing it tries to drain that "body", which blocks
	// for ~10s reading bytes that are actually the start of the tunneled
	// TLS session (that only arrive after we send the ClientHello below) —
	// the proxy then times out the idle connection and the handshake fails.
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: %s", addr, resp.Status)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
