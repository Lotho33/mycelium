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

// BrowserAPIError is a structured non-2xx from the browser service
// ({"error","kind","domain"}).
type BrowserAPIError struct {
	Status int
	Kind   string
	Msg    string
	Domain string
}

func (e *BrowserAPIError) Error() string {
	if e.Kind != "" {
		return fmt.Sprintf("browser service: %s: %s", e.Kind, e.Msg)
	}
	return fmt.Sprintf("browser service: HTTP %d: %s", e.Status, e.Msg)
}

// BrowserClient is the process-wide client of the optional browser service,
// a separate process reached over HTTP/JSON: navigate/eval/sniff/fetch plus
// a readiness probe. It maps stipes-sdk's gen types to that API. Nil until
// ConnectBrowserClient.
var BrowserClient *BrowserServiceClient

// BrowserServiceClient is an HTTP client for the browser service's /v1 API.
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

// ApplyAuth adds the API key to requests built outside this client (the HLS
// proxy's /v1/fetch relay). Safe on a nil client.
func (c *BrowserServiceClient) ApplyAuth(req *http.Request) {
	if c != nil {
		c.setAuth(req)
	}
}

// BaseURL is the browser service's base URL ("" on a nil client).
func (c *BrowserServiceClient) BaseURL() string {
	if c == nil {
		return ""
	}
	return c.baseURL
}

// ConnectBrowserClient points BrowserClient at addr; apiKey is sent as
// X-Api-Key when set. It doesn't fail when the service is unreachable
// (calls fail on their own) but logs a background health check.
func ConnectBrowserClient(addr, apiKey string) error {
	client := &http.Client{Timeout: 65 * time.Second} // above the service's own per-call cap
	// The process-wide resolver only asks public DNS servers, which can't see
	// container names: in Docker, resolve through the container engine's DNS.
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
		// Uses bc, not the package var a later call could replace.
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

// CloseBrowserClient is a no-op (plain HTTP, nothing to close).
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
		return fmt.Errorf("browser service %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		ae := &BrowserAPIError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(respBody))}
		var j struct{ Error, Kind, Domain string }
		if json.Unmarshal(respBody, &j) == nil {
			ae.Kind, ae.Domain = j.Kind, j.Domain
			if j.Error != "" {
				ae.Msg = j.Error
			}
		}
		return fmt.Errorf("browser service %s: %w", path, ae)
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

// Health reports whether the browser service is ready and which engine it
// runs (dashboard status).
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
