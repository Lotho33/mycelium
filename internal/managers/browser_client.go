package managers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Lotho33/stipes-sdk/sdk/gen"

	"mycelium/internal/core"
)

// CobwebAPIError is a structured non-2xx from the browser service
// ({"error","kind","domain"}). The interesting case is
// Kind == "needs_manual_solve": the service landed on an interactive
// verification for Domain that it can't complete on its own — the operator
// completes it once in the interactive browser session.
type CobwebAPIError struct {
	Status int
	Kind   string
	Msg    string
	Domain string
}

func (e *CobwebAPIError) Error() string {
	if e.Kind != "" {
		return fmt.Sprintf("cobweb: %s: %s", e.Kind, e.Msg)
	}
	return fmt.Sprintf("cobweb: HTTP %d: %s", e.Status, e.Msg)
}

// ChallengeDomain returns (domain, true) when err (possibly wrapped) is an
// unresolved interactive-verification error from the browser service.
func ChallengeDomain(err error) (string, bool) {
	var he *CobwebAPIError
	if errors.As(err, &he) && he.Kind == "needs_manual_solve" {
		return he.Domain, true
	}
	return "", false
}

// BrowserClient is the process-wide client for the cobweb browser/extractor
// sidecar (../cobweb, a standalone Rust project — its own repo, own HTTP API,
// no dependency on mycelium) — a separate container, not an in-process
// browser instance. Talks plain HTTP/JSON, deliberately not gRPC: cobweb
// ships HTTP-only (its design keeps mycelium-core free of any Go dependency
// on a cobweb-generated client). Everything else in mycelium (the Lua SDK,
// engine.browserAPI) still speaks in terms of stipes-sdk's gen types —
// BrowserServiceClient just marshals them to/from cobweb's HTTP API instead
// of a local Go call. Nil until ConnectBrowserClient is called.
var BrowserClient *BrowserServiceClient

// BrowserServiceClient is an HTTP client for cobweb's /v1/* API.
type BrowserServiceClient struct {
	baseURL string
	http    *http.Client
}

// ConnectBrowserClient points BrowserClient at addr (cobweb's HTTP base URL,
// e.g. "http://cobweb:8191"). Does not fail if cobweb is unreachable right
// now — every call already fails closed on its own, matching how the rest of
// mycelium's proxy/VPN clients behave — but it does log a one-off warning
// from a background health check so a misconfigured address is visible at
// startup instead of only on the first plugin that needs the browser.
func ConnectBrowserClient(addr string) error {
	client := &http.Client{Timeout: 65 * time.Second} // above cobweb's own 60s per-call cap
	// cmd/server/main.go's init() overrides the process-wide DNS resolver
	// (net.DefaultResolver / http.DefaultTransport) to force lookups through
	// Cloudflare 1.1.1.1 — that resolver can never see Docker-internal names
	// like "cobweb". Same fix already applied to InitRedis/ProxyDialer: when
	// MYCELIUM_DOCKER=1, use a plain Go resolver reading the container
	// engine's own /etc/resolv.conf (127.0.0.11 for Docker) instead of the
	// hardcoded Cloudflare override.
	if os.Getenv("MYCELIUM_DOCKER") == "1" {
		dockerResolver := &net.Resolver{PreferGo: true}
		dockerDialer := &net.Dialer{Timeout: 10 * time.Second, Resolver: dockerResolver}
		client.Transport = &http.Transport{
			DialContext: dockerDialer.DialContext,
		}
	}
	BrowserClient = &BrowserServiceClient{
		baseURL: strings.TrimSuffix(addr, "/"),
		http:    client,
	}
	core.SafeGo("managers/browser-client-health-check", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ready, engine, err := BrowserClient.Health(ctx); err != nil {
			log.Printf("[browser] cobweb at %s not reachable yet: %v (will retry per-call)", addr, err)
		} else {
			log.Printf("[browser] cobweb at %s: ready=%v engine=%s", addr, ready, engine)
		}
	})
	return nil
}

// CloseBrowserClient is a no-op (plain HTTP has no persistent connection to
// close) kept so callers don't need to know the transport changed.
func CloseBrowserClient() {}

func (c *BrowserServiceClient) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cobweb %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		ae := &CobwebAPIError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(respBody))}
		var j struct{ Error, Kind, Domain string }
		if json.Unmarshal(respBody, &j) == nil {
			ae.Kind, ae.Domain = j.Kind, j.Domain
			if j.Error != "" {
				ae.Msg = j.Error
			}
		}
		return fmt.Errorf("cobweb %s: %w", path, ae)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

// NavigateVia navigates url, routing through proxyURL when non-empty.
func (c *BrowserServiceClient) NavigateVia(ctx context.Context, req *gen.NavigateRequest, proxyURL string) (*gen.NavigateResponse, error) {
	var out struct {
		HTML     string `json:"html"`
		FinalURL string `json:"final_url"`
	}
	body := map[string]any{
		"url": req.Url, "wait_for": req.WaitFor, "timeout_ms": req.TimeoutMs, "proxy_url": proxyURL,
	}
	if err := c.post(ctx, "/v1/navigate", body, &out); err != nil {
		return nil, err
	}
	return &gen.NavigateResponse{Html: out.HTML, FinalUrl: out.FinalURL}, nil
}

// EvalVia evaluates JS on url, routing through proxyURL when non-empty.
func (c *BrowserServiceClient) EvalVia(ctx context.Context, req *gen.EvalRequest, proxyURL string) (*gen.EvalResponse, error) {
	var out struct {
		Result string `json:"result"`
	}
	body := map[string]any{
		"url": req.Url, "js": req.Js, "timeout_ms": req.TimeoutMs, "proxy_url": proxyURL,
	}
	if err := c.post(ctx, "/v1/eval", body, &out); err != nil {
		return nil, err
	}
	return &gen.EvalResponse{Result: out.Result}, nil
}

// WaitInterceptVia sniffs network traffic, routing through proxyURL when non-empty.
func (c *BrowserServiceClient) WaitInterceptVia(ctx context.Context, req *gen.WaitInterceptRequest, proxyURL string) (*gen.WaitInterceptResponse, error) {
	var out struct {
		InterceptedURL string            `json:"intercepted_url"`
		Headers        map[string]string `json:"headers"`
	}
	body := map[string]any{
		"trigger_url": req.TriggerUrl, "url_pattern": req.UrlPattern,
		"timeout_ms": req.TimeoutMs, "proxy_url": proxyURL,
	}
	if err := c.post(ctx, "/v1/sniff", body, &out); err != nil {
		return nil, err
	}
	return &gen.WaitInterceptResponse{InterceptedUrl: out.InterceptedURL, Headers: out.Headers}, nil
}

// BaseURL returns cobweb's HTTP base URL (e.g. "http://cobweb:8191") — used
// by the admin manual-session handlers to build the websocket dial target
// and the noVNC static-asset reverse proxy (see internal/api/vpn_session.go).
func (c *BrowserServiceClient) BaseURL() string { return c.baseURL }

// StartSession opens a headed/VNC-watchable browser session on cobweb,
// pointed at targetURL and routed through proxyURL (empty = direct) — see
// ../cobweb's src/vnc/session.rs. Only one may be open at a time on the
// cobweb side (a session older than 20 min is treated as abandoned and the
// slot reclaimed); starting a second while one is genuinely open fails.
func (c *BrowserServiceClient) StartSession(ctx context.Context, targetURL, proxyURL string) (id string, err error) {
	var out struct {
		ID        string `json:"id"`
		TargetURL string `json:"target_url"`
	}
	body := map[string]any{"url": targetURL, "proxy_url": proxyURL}
	if err := c.post(ctx, "/v1/session/start", body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// CloseSession tears down the headed browser + VNC server. saveCookies=true
// persists the session's cookie jar on the cobweb side (reused by every later
// automated Navigate/Eval/WaitIntercept against the same domain); false
// discards it — for when the session was just a look-around and its cookies
// would only pollute the jar for a domain that doesn't need them. cobweb reads
// "save_cookies" from the body and defaults to true when the field is absent
// (older callers).
func (c *BrowserServiceClient) CloseSession(ctx context.Context, id string, saveCookies bool) error {
	return c.post(ctx, "/v1/session/"+id+"/close", map[string]any{"save_cookies": saveCookies}, nil)
}

// CurrentSession reports the manual session cobweb currently has open, if
// any — the id otherwise lives only in the admin browser's own JS state, so
// losing it (page refresh, a dropped vnc-ws websocket) left no way to either
// resume or close a lingering session short of restarting cobweb.
func (c *BrowserServiceClient) CurrentSession(ctx context.Context) (id, targetURL string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/session/current", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var out struct {
		ID        string `json:"id"`
		TargetURL string `json:"target_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.ID, out.TargetURL, nil
}

// JarEntry is one row of cobweb's cookie/challenge jar (GET /v1/jar). Fields
// are passed straight through to the admin dashboard, which renders the list
// and a "re-solve" action per stale entry.
type JarEntry struct {
	Domain      string  `json:"domain"`
	Egress      string  `json:"egress"`
	Stale       bool    `json:"stale"`
	Created     string  `json:"created"`
	ExpiresAt   string  `json:"expires_at"`
	LastOK      *string `json:"last_ok"`
	TTLHintSecs int64   `json:"ttl_hint_secs"`
	FailStreak  int     `json:"fail_streak"`
	CookieCount int     `json:"cookie_count"`
}

// JarList returns every entry in cobweb's jar — the domains for which a
// challenge/login has been solved, with freshness so the dashboard can flag
// the ones due for a re-solve.
func (c *BrowserServiceClient) JarList(ctx context.Context) ([]JarEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/jar", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cobweb /v1/jar: HTTP %d", resp.StatusCode)
	}
	var out []JarEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// JarDelete drops the stored state for one (domain, egressKey) — forces the
// next request against it to re-solve. cobweb keys jar files by domain AND
// egress key (a profile name, "direct", or "proxy-<host>-<port>"), so
// egressKey must be the value from the JarEntry, else nothing matches (cobweb
// defaults the missing key to "direct" and silently removes nothing).
// Returns whether a file was actually removed.
func (c *BrowserServiceClient) JarDelete(ctx context.Context, domain, egressKey string) (bool, error) {
	u := c.baseURL + "/v1/jar/" + url.PathEscape(domain)
	if egressKey != "" {
		u += "?egress=" + url.QueryEscape(egressKey)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return false, fmt.Errorf("cobweb DELETE /v1/jar/%s: HTTP %d", domain, resp.StatusCode)
	}
	var out struct {
		Removed bool `json:"removed"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Removed, nil
}

// Health reports whether cobweb's browser engine is up and which engine it's
// running (e.g. "webkit") — used by the admin dashboard.
func (c *BrowserServiceClient) Health(ctx context.Context) (ready bool, engine string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return false, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	var out struct {
		Ready  bool   `json:"ready"`
		Engine string `json:"engine"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, "", err
	}
	return out.Ready, out.Engine, nil
}
