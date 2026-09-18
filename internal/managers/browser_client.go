package managers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Lotho33/stipes-sdk/sdk/gen"

	"mycelium/internal/core"
)

// CobwebAPIError is a structured non-2xx from the browser service
// ({"error","kind","domain"}).
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

// BrowserClient is the process-wide client for the browser/extractor
// sidecar — a separate container, not an in-process browser instance. Talks
// plain HTTP/JSON. Everything else in mycelium (the Lua SDK,
// engine.browserAPI) still speaks in terms of stipes-sdk's gen types —
// BrowserServiceClient just marshals them to/from the sidecar's HTTP API
// instead of a local Go call. Nil until ConnectBrowserClient is called.
//
// mycelium-core deliberately knows nothing about this service beyond a
// generic HTTP contract: no admin-dashboard settings/blocklist surface
// naming or configuring it lives here (that stays entirely on the sidecar's
// own side, configured directly there) — only the navigate/eval/sniff calls
// every content-source plugin needs, plus a bare readiness probe.
var BrowserClient *BrowserServiceClient

// BrowserServiceClient is an HTTP client for the browser sidecar's /v1/* API.
type BrowserServiceClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// setAuth attaches the configured API key, if any, to req.
func (c *BrowserServiceClient) setAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
}

// ConnectBrowserClient points BrowserClient at addr (the sidecar's HTTP
// base URL). apiKey is sent as X-Api-Key on every request; leave it empty
// if the sidecar has no API key configured (its auth middleware then
// accepts unauthenticated requests). Does not fail if the sidecar is
// unreachable right now — every call already fails closed on its own,
// matching how the rest of mycelium's proxy/VPN clients behave — but it
// does log a one-off warning from a background health check so a
// misconfigured address is visible at startup instead of only on the first
// plugin that needs the browser.
func ConnectBrowserClient(addr, apiKey string) error {
	client := &http.Client{Timeout: 65 * time.Second} // above the sidecar's own 60s per-call cap
	// cmd/server/main.go's init() overrides the process-wide DNS resolver
	// (net.DefaultResolver / http.DefaultTransport) to force lookups through
	// Cloudflare 1.1.1.1 — that resolver can never see Docker-internal names.
	// Same fix already applied to InitRedis/ProxyDialer: when
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
	bc := &BrowserServiceClient{
		baseURL: strings.TrimSuffix(addr, "/"),
		apiKey:  apiKey,
		http:    client,
	}
	BrowserClient = bc
	core.SafeGo("managers/browser-client-health-check", func() {
		// Closes over bc, the client just created — NOT the package-level
		// BrowserClient var, which a later ConnectBrowserClient call (tests
		// do this repeatedly; production never reassigns it after startup)
		// could have already pointed elsewhere or nil'd out by the time this
		// fires, up to 5s later.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ready, engine, err := bc.Health(ctx); err != nil {
			log.Printf("[browser] extractor at %s not reachable yet: %v (will retry per-call)", addr, err)
		} else {
			log.Printf("[browser] extractor at %s: ready=%v engine=%s", addr, ready, engine)
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
	c.setAuth(req)
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

// Health reports whether the browser sidecar's engine is up and which
// engine it's running — used for the dashboard's generic "Extractor"
// status indicator (never names the sidecar itself, just ready/engine).
func (c *BrowserServiceClient) Health(ctx context.Context) (ready bool, engine string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return false, "", err
	}
	c.setAuth(req)
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
