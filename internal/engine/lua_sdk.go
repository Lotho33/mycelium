package engine

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mycelium/internal/managers"

	"github.com/Lotho33/stipes-sdk/sdk/gen"
	"github.com/PuerkitoBio/goquery"
	"github.com/tidwall/gjson"
	lua "github.com/yuin/gopher-lua"
)

// ProgressFunc receives a status ("", "success", "warning", "error") and a
// free-form, plugin-owned message from mycelium.progress() Lua calls.
type ProgressFunc func(status, message string)

// browserAPI is the subset of the browser/extractor service the Lua SDK
// needs — satisfied today by *managers.BrowserServiceClient, an HTTP client
// for cobweb's API (see internal/managers/browser_client.go). Named as an interface
// rather than depending on the concrete client type directly so the browser
// engine can move to a genuinely separate service later without touching
// call sites here — only what ConnectBrowserClient dials would change.
type browserAPI interface {
	NavigateVia(ctx context.Context, req *gen.NavigateRequest, proxyURL string) (*gen.NavigateResponse, error)
	EvalVia(ctx context.Context, req *gen.EvalRequest, proxyURL string) (*gen.EvalResponse, error)
	WaitInterceptVia(ctx context.Context, req *gen.WaitInterceptRequest, proxyURL string) (*gen.WaitInterceptResponse, error)
}

// currentBrowserClient returns managers.BrowserClient as a browserAPI, or a
// true nil interface if it hasn't connected yet. Assigning a nil
// *managers.BrowserServiceClient directly to an interface field would
// produce a non-nil interface wrapping a nil pointer (the classic Go "typed
// nil" trap), silently breaking the "browser service not available" check
// in buildBrowserModule below.
func currentBrowserClient() browserAPI {
	if managers.BrowserClient == nil {
		return nil
	}
	return managers.BrowserClient
}

// maxSleepMillis caps mycelium.sleep(ms) at the same order of magnitude as
// defaultEntrypointTimeout (lua_plugin.go) — a plugin has no legitimate reason
// to sleep past its own call budget, and capping here bounds how long an
// abandoned post-timeout goroutine can be kept alive by a single sleep call
// even in the ctx == nil fallback path (no entrypoint deadline to race
// against).
const maxSleepMillis = int(defaultEntrypointTimeout / time.Millisecond)

// maxSDKResponseBytes caps how much of an HTTP response body
// mycelium.network.get/post/fetch will read into memory. Without it, a
// compromised or merely malformed scrape target (a CDN serving a
// multi-gigabyte file where a small JSON/HTML page was expected) gets read in
// full by io.Copy into an unbounded bytes.Buffer — a silent memory DoS.
// 32 MiB comfortably covers any legitimate catalog/page payload plugins parse
// today while keeping a single runaway response cheap to discard.
const maxSDKResponseBytes = 32 << 20 // 32 MiB

// maxLogoImageBytes caps the source image mycelium.image.analyze_logo decodes.
// Logos are small, deliberately-chosen brand assets (the module targets an
// 800×200 normalized size) — a dedicated, smaller cap than
// maxSDKResponseBytes catches a mismatched/hostile URL (e.g. a full video
// file) before image.Decode allocates a full in-memory frame for it.
const maxLogoImageBytes = 8 << 20 // 8 MiB

// maxCacheValueBytes caps the serialized size of a single mycelium.cache.set
// value before it is written to Redis. cache.set entries are meant for a
// plugin's own API/catalog responses it wants to reuse across calls — not
// arbitrary blobs — and a call can also pass ttl=0 (no expiry), so without a
// cap a plugin (buggy or hostile) can grow the shared Redis instance without
// bound simply by writing large values repeatedly. 4 MiB comfortably covers
// any legitimate catalog/page payload (well under maxSDKResponseBytes, the
// cap on the HTTP fetch that would have produced it) while keeping a single
// key cheap to store and evict.
const maxCacheValueBytes = 4 << 20 // 4 MiB

// maxStorageWriteBytes caps the serialized size of a single
// mycelium.storage.write_json payload before it is written to disk. Storage
// files live in the plugin's own directory and can reasonably hold more than
// one Redis cache entry (e.g. a plugin's full local catalog dump), so the cap
// sits above maxCacheValueBytes — but still well under maxSDKResponseBytes,
// since it is app-level JSON a plugin builds itself, not an arbitrary
// downloaded file. Without this cap a plugin can fill its own plugin
// directory (and, in aggregate, the host disk) with unbounded writes.
const maxStorageWriteBytes = 16 << 20 // 16 MiB

// SDKOpts carries the per-PLUGIN invariants injected into the Lua SDK modules.
// It is passed once, when a pooled LState is built (RegisterSDK). The bits that
// vary per entrypoint call — profile id, whether this call must be forced onto
// the VPN proxy, and the streaming-progress sink — live in callScope instead
// (see lua_scope.go), so the module tables and closures are built once and
// never rebuilt on the hot path.
type SDKOpts struct {
	PluginID        string
	PluginDir       string
	Redis           *managers.RedisRepo
	Browser         browserAPI
	LogBuf          *managers.PluginLogBuffer
	HTTPClient      *http.Client // direct client (no proxy)
	ProxyHTTPClient *http.Client // legacy single-proxy client; kept for callers that don't set ProxyClientFor
	// ProxyURL is the raw legacy VPN/proxy URL (e.g. socks5://warp:1080).
	ProxyURL string
	// ProxyClientFor returns a scraping http.Client for an egress proxy URL
	// (the per-call callScope.proxyURL, resolved from the plugin's selected
	// egress profile). nil-safe: falls back to ProxyHTTPClient.
	ProxyClientFor func(proxyURL string) *http.Client
}

// scopeProxyClient picks the http.Client for a proxied call: the per-egress
// one from ProxyClientFor(scope.proxyURL), else the legacy ProxyHTTPClient.
//
// A requireProxy call with an EMPTY proxyURL means the plugin's selected egress
// is registered but currently disabled — pluginCallProxy's fail-closed signal.
// It must return nil (→ the SDK blocks the call), NOT silently fall back to the
// legacy global proxy. The ProxyHTTPClient fallback is only for the best-effort
// opt-in path, where an empty proxyURL just means "no specific egress picked".
func (o SDKOpts) scopeProxyClient(scope *callScope) *http.Client {
	if scope.requireProxy && scope.proxyURL == "" {
		return nil
	}
	if o.ProxyClientFor != nil {
		if c := o.ProxyClientFor(scope.proxyURL); c != nil {
			return c
		}
	}
	if scope.proxyURL == "" || scope.proxyURL == o.ProxyURL {
		return o.ProxyHTTPClient
	}
	return nil
}

// RegisterSDK registers the mycelium.* modules on L. Called once per pooled
// LState. `scope` is that state's callScope — the SDK closures read the
// current call's profile id / proxy requirement / progress sink from it.
func RegisterSDK(L *lua.LState, opts SDKOpts, scope *callScope) {
	mycelium := L.NewTable()

	mycelium.RawSetString("network", buildNetworkModule(L, opts, scope))
	mycelium.RawSetString("dom", buildDOMModule(L))
	mycelium.RawSetString("json", buildJSONModule(L))
	mycelium.RawSetString("context", buildContextModule(L, opts, scope))
	mycelium.RawSetString("browser", buildBrowserModule(L, opts, scope))
	mycelium.RawSetString("crypto", buildCryptoModule(L))
	mycelium.RawSetString("cache", buildCacheModule(L, opts))
	mycelium.RawSetString("xml", buildXMLModule(L))
	mycelium.RawSetString("storage", buildStorageModule(L, opts))
	mycelium.RawSetString("image", buildImageModule(L, opts))
	mycelium.RawSetString("log", L.NewFunction(func(L *lua.LState) int {
		msg := L.OptString(1, "")
		formatted := "[lua:" + opts.PluginID + "] " + msg
		if opts.LogBuf != nil {
			opts.LogBuf.Append(formatted)
		}
		log.Print(formatted)
		return 0
	}))

	// progress(status, message) — informational update while the plugin is still
	// working (e.g. inside resolve_stream). status is one of "", "success",
	// "warning", "error" (picks an icon client-side; free-form text stays in
	// message). No-op when the current call doesn't support streaming progress.
	mycelium.RawSetString("progress", L.NewFunction(func(L *lua.LState) int {
		statusStr := L.OptString(1, "")
		msg := L.OptString(2, "")
		if scope.onProgress != nil {
			scope.onProgress(statusStr, msg)
		}
		return 0
	}))

	// mycelium.sleep(ms) — blocks the calling Lua goroutine for up to ms
	// milliseconds, capped at maxSleepMillis and cut short the moment the
	// current entrypoint's context is done (its normal deadline, or an early
	// cancellation). Without this, a plugin calling mycelium.sleep(hugeNumber)
	// from a function that the 90s entrypoint timeout later abandons
	// (callWithTimeout in lua_plugin.go never stops the goroutine, it just
	// stops waiting on it) would keep that goroutine — and the *lua.LState it
	// holds, discarded from the pool but not GC-able while still referenced —
	// alive for the full requested sleep, unboundedly on every repeated call.
	mycelium.RawSetString("sleep", L.NewFunction(func(L *lua.LState) int {
		ms := L.OptInt(1, 0)
		if ms <= 0 {
			return 0
		}
		if ms > maxSleepMillis {
			ms = maxSleepMillis
		}
		ctx := scope.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-ctx.Done():
		}
		return 0
	}))

	L.SetGlobal("mycelium", mycelium)
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.network
// ─────────────────────────────────────────────────────────────────────────────

func buildNetworkModule(L *lua.LState, opts SDKOpts, scope *callScope) *lua.LTable {
	t := L.NewTable()

	doHTTP := func(method, rawURL, bodyStr string, headers *lua.LTable, timeoutSec int) *lua.LTable {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
		defer cancel()

		// Built once, applied fresh to each retry attempt's request below.
		optInProxy := false
		extraHeaders := map[string]string{}
		if headers != nil {
			headers.ForEach(func(k, v lua.LValue) {
				key := k.String()
				if key == "_proxy" {
					optInProxy = lua.LVAsBool(v)
					return
				}
				extraHeaders[key] = v.String()
			})
		}

		// Client selection.
		//   - VPN-routed plugins (RequireProxy, the default): ALWAYS through the proxy,
		//     fail closed if none is configured. The _proxy header cannot opt out.
		//   - direct_egress plugins: _proxy=true is a best-effort opt-in — use the proxy
		//     when available, otherwise fall back to the direct client (backward compatible).
		client := opts.HTTPClient
		if scope.requireProxy {
			pc := opts.scopeProxyClient(scope)
			if pc == nil {
				res := L.NewTable()
				res.RawSetString("status_code", lua.LNumber(0))
				res.RawSetString("body", lua.LString(""))
				res.RawSetString("error", lua.LString("plugin requires VPN but the selected egress is unavailable"))
				return res
			}
			client = pc
		} else if optInProxy {
			if pc := opts.scopeProxyClient(scope); pc != nil {
				client = pc
			}
		}
		if client == nil {
			client = http.DefaultClient
		}

		// Transient transport-level failures (SOCKS/VPN hiccups, DNS blips,
		// a CDN edge briefly down) get retried a couple of times before giving
		// up — a single failed connection attempt otherwise surfaced straight
		// to Lua as status_code=0, and plugins generally don't retry
		// themselves (e.g. a plugin's metadata-list fetch). Requests
		// are idempotent GET/HEAD-shaped API calls in practice, so retrying
		// is safe; HTTP-level responses (even error statuses) are returned
		// as-is on the first attempt, not retried.
		const maxAttempts = 3
		var resp *http.Response
		var err error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			var bodyReader io.Reader
			if bodyStr != "" {
				bodyReader = strings.NewReader(bodyStr)
			}
			var req *http.Request
			req, err = http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
			if err != nil {
				res := L.NewTable()
				res.RawSetString("status_code", lua.LNumber(0))
				res.RawSetString("body", lua.LString(""))
				res.RawSetString("error", lua.LString(err.Error()))
				return res
			}
			for k, v := range extraHeaders {
				req.Header.Set(k, v)
			}
			if req.Header.Get("User-Agent") == "" {
				req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36")
			}

			resp, err = client.Do(req)
			if err == nil {
				break
			}
			if attempt < maxAttempts && ctx.Err() == nil {
				log.Printf("[sdk/network] %s %s → error (attempt %d/%d): %v", method, rawURL, attempt, maxAttempts, err)
				time.Sleep(time.Duration(attempt) * 150 * time.Millisecond)
				continue
			}
		}
		if err != nil {
			log.Printf("[sdk/network] %s %s → error: %v", method, rawURL, err)
			res := L.NewTable()
			res.RawSetString("status_code", lua.LNumber(0))
			res.RawSetString("body", lua.LString(""))
			res.RawSetString("error", lua.LString(err.Error()))
			return res
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, io.LimitReader(resp.Body, maxSDKResponseBytes))

		respHeaders := L.NewTable()
		for k, vs := range resp.Header {
			if len(vs) > 0 {
				// Join multiple values with \n so Lua can split them (important for Set-Cookie)
				respHeaders.RawSetString(k, lua.LString(strings.Join(vs, "\n")))
			}
		}
		res := L.NewTable()
		res.RawSetString("status_code", lua.LNumber(resp.StatusCode))
		res.RawSetString("body", lua.LString(buf.String()))
		res.RawSetString("headers", respHeaders)
		res.RawSetString("error", lua.LNil)
		return res
	}

	// mycelium.network.get(url [, headers_table [, timeout_seconds]]) → resp_table
	t.RawSetString("get", L.NewFunction(func(L *lua.LState) int {
		rawURL := L.CheckString(1)
		headers := L.OptTable(2, nil)
		timeout := L.OptInt(3, 30)
		L.Push(doHTTP("GET", rawURL, "", headers, timeout))
		return 1
	}))

	// mycelium.network.post(url, body [, headers_table [, timeout_seconds]]) → resp_table
	t.RawSetString("post", L.NewFunction(func(L *lua.LState) int {
		rawURL := L.CheckString(1)
		body := L.CheckString(2)
		headers := L.OptTable(3, nil)
		timeout := L.OptInt(4, 30)
		L.Push(doHTTP("POST", rawURL, body, headers, timeout))
		return 1
	}))

	// mycelium.network.fetch(url [, opts_table]) → body_string, err_string  (backward compat)
	t.RawSetString("fetch", L.NewFunction(func(L *lua.LState) int {
		rawURL := L.CheckString(1)
		optsTable := L.OptTable(2, L.NewTable())

		method := strings.ToUpper(optStr(optsTable, "method", "GET"))
		bodyStr := optStr(optsTable, "body", "")
		timeoutSec := optInt(optsTable, "timeout_seconds", 30)

		var hdrs *lua.LTable
		if h, ok := optsTable.RawGetString("headers").(*lua.LTable); ok {
			hdrs = h
		}
		res := doHTTP(method, rawURL, bodyStr, hdrs, timeoutSec)
		L.Push(res.RawGetString("body"))
		L.Push(res.RawGetString("error"))
		return 2
	}))

	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.dom
// ─────────────────────────────────────────────────────────────────────────────

func buildDOMModule(L *lua.LState) *lua.LTable {
	t := L.NewTable()

	// mycelium.dom.query(html, css) → text of first match
	t.RawSetString("query", L.NewFunction(func(L *lua.LState) int {
		html := L.CheckString(1)
		selector := L.CheckString(2)
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
		if err != nil {
			L.Push(lua.LString(""))
			return 1
		}
		L.Push(lua.LString(strings.TrimSpace(doc.Find(selector).First().Text())))
		return 1
	}))

	// mycelium.dom.query_all(html, css) → array of text strings
	t.RawSetString("query_all", L.NewFunction(func(L *lua.LState) int {
		html := L.CheckString(1)
		selector := L.CheckString(2)
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
		if err != nil {
			L.Push(L.NewTable())
			return 1
		}
		arr := L.NewTable()
		doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
			arr.Append(lua.LString(strings.TrimSpace(s.Text())))
		})
		L.Push(arr)
		return 1
	}))

	// mycelium.dom.select(html, css) → array of {text, html, attr=fn}  (backward compat)
	t.RawSetString("select", L.NewFunction(func(L *lua.LState) int {
		html := L.CheckString(1)
		selector := L.CheckString(2)
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		results := L.NewTable()
		doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
			node := L.NewTable()
			node.RawSetString("text", lua.LString(strings.TrimSpace(s.Text())))
			outerHTML, _ := goquery.OuterHtml(s)
			node.RawSetString("html", lua.LString(outerHTML))
			node.RawSetString("attr", L.NewFunction(func(L *lua.LState) int {
				name := L.CheckString(1)
				val, _ := s.Attr(name)
				L.Push(lua.LString(val))
				return 1
			}))
			results.Append(node)
		})
		L.Push(results)
		L.Push(lua.LNil)
		return 2
	}))

	// mycelium.dom.attr(html, css, attr_name) → string
	t.RawSetString("attr", L.NewFunction(func(L *lua.LState) int {
		html := L.CheckString(1)
		selector := L.CheckString(2)
		attrName := L.CheckString(3)
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
		if err != nil {
			L.Push(lua.LString(""))
			return 1
		}
		val, _ := doc.Find(selector).First().Attr(attrName)
		L.Push(lua.LString(val))
		return 1
	}))

	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.json
// ─────────────────────────────────────────────────────────────────────────────

func buildJSONModule(L *lua.LState) *lua.LTable {
	t := L.NewTable()

	// mycelium.json.parse(json_string) → lua_table, err
	t.RawSetString("parse", L.NewFunction(func(L *lua.LState) int {
		jsonStr := L.CheckString(1)
		var raw any
		if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(goToLua(L, raw))
		L.Push(lua.LNil)
		return 2
	}))

	// mycelium.json.stringify(table_or_value) → json_string
	t.RawSetString("stringify", L.NewFunction(func(L *lua.LState) int {
		val := L.CheckAny(1)
		b, err := json.Marshal(luaToGo(val))
		if err != nil {
			L.Push(lua.LString("null"))
			return 1
		}
		L.Push(lua.LString(string(b)))
		return 1
	}))

	// mycelium.json.get(json_string, path) → value  (gjson fast-path, backward compat)
	t.RawSetString("get", L.NewFunction(func(L *lua.LState) int {
		jsonStr := L.CheckString(1)
		path := L.CheckString(2)
		result := gjson.Get(jsonStr, path)
		if !result.Exists() {
			L.Push(lua.LNil)
			return 1
		}
		switch result.Type {
		case gjson.String:
			L.Push(lua.LString(result.String()))
		case gjson.Number:
			L.Push(lua.LNumber(result.Float()))
		case gjson.True:
			L.Push(lua.LTrue)
		case gjson.False:
			L.Push(lua.LFalse)
		default:
			L.Push(lua.LString(result.Raw))
		}
		return 1
	}))

	// mycelium.json.get_array(json_string, path) → lua array of raw strings  (backward compat)
	t.RawSetString("get_array", L.NewFunction(func(L *lua.LState) int {
		jsonStr := L.CheckString(1)
		path := L.CheckString(2)
		arr := L.NewTable()
		gjson.Get(jsonStr, path).ForEach(func(_, v gjson.Result) bool {
			arr.Append(lua.LString(v.Raw))
			return true
		})
		L.Push(arr)
		return 1
	}))

	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.browser
// ─────────────────────────────────────────────────────────────────────────────

// egressLabel is a short tag for the challenge registry: "direct" for the
// box's own IP, else the proxy's host.
func egressLabel(proxyURL string) string {
	if proxyURL == "" {
		return "direct"
	}
	s := proxyURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// regDomainOf is a PSL-free registrable-domain guess (last two dot labels) —
// good enough for the single-label TLDs common among stream CDNs. Used only to
// clear a pending verification entry on a later clean sniff.
func regDomainOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	p := strings.Split(strings.ToLower(strings.Trim(s, ".")), ".")
	if len(p) < 2 {
		return s
	}
	return p[len(p)-2] + "." + p[len(p)-1]
}

func buildBrowserModule(L *lua.LState, opts SDKOpts, scope *callScope) *lua.LTable {
	t := L.NewTable()
	bm := opts.Browser

	// VPN-routed calls send their browser traffic through the VPN too. Whether
	// this call is VPN-routed depends on callScope (set per entrypoint), so it
	// is resolved per invocation, not once at build time. blocked == true means
	// "this call must use the proxy but none is configured".
	proxyFor := func() (proxyURL string, blocked bool) {
		if !scope.requireProxy {
			return "", false
		}
		if scope.proxyURL == "" {
			return "", true // selected egress unavailable → block, don't leak direct
		}
		return scope.proxyURL, false
	}

	// mycelium.browser.sniff(trigger_url, url_pattern, timeout_seconds) → intercepted_url, headers, err
	// headers is a Lua table of {[name]=value} captured from the intercepted request
	// (useful for replaying streams that need specific auth/cookie headers).
	t.RawSetString("sniff", L.NewFunction(func(L *lua.LState) int {
		triggerURL := L.CheckString(1)
		pattern := L.CheckString(2)
		timeoutSec := L.OptInt(3, 30)
		if bm == nil {
			L.Push(lua.LNil)
			L.Push(lua.LNil)
			L.Push(lua.LString("browser service not available"))
			return 3
		}
		proxyURL, blocked := proxyFor()
		if blocked {
			L.Push(lua.LNil)
			L.Push(lua.LNil)
			L.Push(lua.LString("plugin requires VPN but no proxy is configured"))
			return 3
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
		defer cancel()
		resp, err := bm.WaitInterceptVia(ctx, &gen.WaitInterceptRequest{
			TriggerUrl: triggerURL,
			UrlPattern: pattern,
			TimeoutMs:  int64(timeoutSec) * 1000,
		}, proxyURL)
		if err != nil {
			// The browser service hit an interactive verification it can't
			// clear on its own — record it so the dashboard/Pileus can ask the
			// operator to complete it in a browser.
			if dom, ok := managers.ChallengeDomain(err); ok {
				managers.RecordChallenge(dom, egressLabel(proxyURL))
				log.Printf("[sdk/browser] sniff %s → verification required on %s (egress %s)",
					opts.PluginID, dom, egressLabel(proxyURL))
				L.Push(lua.LNil)
				L.Push(lua.LNil)
				L.Push(lua.LString("CHALLENGE:" + dom + " " + err.Error()))
				return 3
			}
			L.Push(lua.LNil)
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 3
		}
		// A clean sniff means any pending verification for this host is cleared.
		managers.ClearChallengeDomain(regDomainOf(triggerURL))
		hdrs := L.NewTable()
		hdrKeys := make([]string, 0, len(resp.Headers))
		for k, v := range resp.Headers {
			hdrs.RawSetString(k, lua.LString(v))
			hdrKeys = append(hdrKeys, k)
		}
		// Log which header names the sniff returned (values elided; cookie
		// length only) so a plugin author can see what the browser service
		// handed back.
		log.Printf("[sdk/browser] sniff %s → intercepted=%q header_keys=%v cookie_len=%d",
			opts.PluginID, resp.InterceptedUrl, hdrKeys, len(resp.Headers["cookie"])+len(resp.Headers["Cookie"]))
		L.Push(lua.LString(resp.InterceptedUrl))
		L.Push(hdrs)
		L.Push(lua.LNil)
		return 3
	}))

	// mycelium.browser.navigate(url [, timeout_seconds]) → html, final_url, err
	t.RawSetString("navigate", L.NewFunction(func(L *lua.LState) int {
		url := L.CheckString(1)
		timeoutSec := L.OptInt(2, 30)
		if bm == nil {
			L.Push(lua.LNil)
			L.Push(lua.LNil)
			L.Push(lua.LString("browser service not available"))
			return 3
		}
		proxyURL, blocked := proxyFor()
		if blocked {
			L.Push(lua.LNil)
			L.Push(lua.LNil)
			L.Push(lua.LString("plugin requires VPN but no proxy is configured"))
			return 3
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
		defer cancel()
		resp, err := bm.NavigateVia(ctx, &gen.NavigateRequest{
			Url:       url,
			TimeoutMs: int64(timeoutSec) * 1000,
		}, proxyURL)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 3
		}
		L.Push(lua.LString(resp.Html))
		L.Push(lua.LString(resp.FinalUrl))
		L.Push(lua.LNil)
		return 3
	}))

	// mycelium.browser.eval(url, js_code [, timeout_seconds]) → result_string, err
	t.RawSetString("eval", L.NewFunction(func(L *lua.LState) int {
		url := L.CheckString(1)
		js := L.CheckString(2)
		timeoutSec := L.OptInt(3, 30)
		if bm == nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("browser service not available"))
			return 2
		}
		proxyURL, blocked := proxyFor()
		if blocked {
			L.Push(lua.LNil)
			L.Push(lua.LString("plugin requires VPN but no proxy is configured"))
			return 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
		defer cancel()
		resp, err := bm.EvalVia(ctx, &gen.EvalRequest{
			Url:       url,
			Js:        js,
			TimeoutMs: int64(timeoutSec) * 1000,
		}, proxyURL)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LString(resp.Result))
		L.Push(lua.LNil)
		return 2
	}))

	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.crypto
// ─────────────────────────────────────────────────────────────────────────────

func buildCryptoModule(L *lua.LState) *lua.LTable {
	t := L.NewTable()

	// mycelium.crypto.base64_decode(b64_string) → decoded_string, err
	t.RawSetString("base64_decode", L.NewFunction(func(L *lua.LState) int {
		enc := L.CheckString(1)
		// Try standard, then URL, then raw (no-padding) encodings.
		data, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			data, err = base64.URLEncoding.DecodeString(enc)
		}
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(enc)
		}
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LString(string(data)))
		L.Push(lua.LNil)
		return 2
	}))

	// mycelium.crypto.aes_decrypt(ciphertext_b64, key_hex, iv_hex) → plaintext_string, err
	// AES-128 or AES-256 CBC; ciphertext is base64-encoded.
	t.RawSetString("aes_decrypt", L.NewFunction(func(L *lua.LState) int {
		ciphertextB64 := L.CheckString(1)
		keyHex := L.CheckString(2)
		ivHex := L.CheckString(3)

		ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
		if err != nil {
			ciphertext, err = base64.RawStdEncoding.DecodeString(ciphertextB64)
		}
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("ciphertext base64 decode: " + err.Error()))
			return 2
		}
		key, err := hex.DecodeString(keyHex)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("key hex decode: " + err.Error()))
			return 2
		}
		iv, err := hex.DecodeString(ivHex)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("iv hex decode: " + err.Error()))
			return 2
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("aes new cipher: " + err.Error()))
			return 2
		}
		if len(ciphertext)%aes.BlockSize != 0 {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("ciphertext length %d not a block-size multiple", len(ciphertext))))
			return 2
		}
		plain := make([]byte, len(ciphertext))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ciphertext)
		// Strip PKCS#7 padding.
		if n := len(plain); n > 0 {
			pad := int(plain[n-1])
			if pad > 0 && pad <= aes.BlockSize && n >= pad {
				plain = plain[:n-pad]
			}
		}
		L.Push(lua.LString(string(plain)))
		L.Push(lua.LNil)
		return 2
	}))

	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.cache  (Redis TTL, plugin-namespaced)
// ─────────────────────────────────────────────────────────────────────────────

func buildCacheModule(L *lua.LState, opts SDKOpts) *lua.LTable {
	t := L.NewTable()

	cacheKey := func(key string) string {
		return "mycelium:plugin:" + opts.PluginID + ":cache:" + key
	}

	// mycelium.cache.set(key, value, ttl_seconds) → ok_bool, err_string
	// The two return values are optional for callers (existing call sites
	// across every plugin ignore them, which is valid Lua) — but a write
	// failure is no longer invisible for callers that DO check: a plugin's
	// own catalog can silently get stuck re-syncing the same stale range
	// forever if a critical write like this one is dropped with nothing
	// ever finding out — a plugin that persists sync progress should check
	// the return value.
	t.RawSetString("set", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val := L.CheckString(2)
		ttl := L.OptInt(3, 0)
		if len(val) > maxCacheValueBytes {
			L.Push(lua.LFalse)
			L.Push(lua.LString(fmt.Sprintf("cache.set: value too large (%d bytes, max %d)", len(val), maxCacheValueBytes)))
			return 2
		}
		if opts.Redis == nil {
			L.Push(lua.LFalse)
			L.Push(lua.LString("redis unavailable"))
			return 2
		}
		if err := opts.Redis.Set(context.Background(), cacheKey(key), val, time.Duration(ttl)*time.Second); err != nil {
			L.Push(lua.LFalse)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LTrue)
		L.Push(lua.LNil)
		return 2
	}))

	// mycelium.cache.get(key) → value_string, found_bool
	t.RawSetString("get", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		if opts.Redis == nil {
			L.Push(lua.LString(""))
			L.Push(lua.LFalse)
			return 2
		}
		val, err := opts.Redis.Get(context.Background(), cacheKey(key))
		if err != nil || val == "" {
			L.Push(lua.LString(""))
			L.Push(lua.LFalse)
			return 2
		}
		L.Push(lua.LString(val))
		L.Push(lua.LTrue)
		return 2
	}))

	// mycelium.cache.del(key)
	t.RawSetString("del", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		if opts.Redis != nil {
			_ = opts.Redis.Del(context.Background(), cacheKey(key))
		}
		return 0
	}))

	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.context  (per-profile secrets via Redis)
// ─────────────────────────────────────────────────────────────────────────────

func buildContextModule(L *lua.LState, opts SDKOpts, scope *callScope) *lua.LTable {
	t := L.NewTable()

	t.RawSetString("get_secret", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		if opts.Redis == nil || scope.profileID == "" {
			L.Push(lua.LString(""))
			return 1
		}
		val, _ := opts.Redis.HGet(context.Background(), managers.SecretsKey(opts.PluginID, scope.profileID), key)
		L.Push(lua.LString(val))
		return 1
	}))

	t.RawSetString("set_secret", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val := L.CheckString(2)
		if opts.Redis == nil || scope.profileID == "" {
			return 0
		}
		_ = opts.Redis.HSet(context.Background(), managers.SecretsKey(opts.PluginID, scope.profileID), map[string]any{key: val})
		return 0
	}))

	// mycelium.context.get_profile_id() → profile_id string
	t.RawSetString("get_profile_id", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(scope.profileID))
		return 1
	}))

	// mycelium.context.get_global_setting(key) → value string
	// Reads a global (admin-configured) plugin setting stored under lua:{pluginID}:global:{key}.
	t.RawSetString("get_global_setting", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val := managers.Settings.GetString("lua:"+opts.PluginID+":global:"+key, "")
		L.Push(lua.LString(val))
		return 1
	}))

	// mycelium.context.set_plugin_status(label, detail)
	// label: "ready"|"syncing"|"needs_config"|"error"
	// detail: human-readable description (empty string to clear)
	t.RawSetString("set_plugin_status", L.NewFunction(func(L *lua.LState) int {
		label := L.CheckString(1)
		detail := L.OptString(2, "")
		LuaPlugins.SetStatus(opts.PluginID, label, detail)
		return 0
	}))

	t.RawSetString("plugin_id", lua.LString(opts.PluginID))

	// mycelium.context.profile_id — a convenience field, kept live via an
	// __index metatable (the table is built once now, so a static value would
	// freeze to ""). Real keys set above bypass __index; only the missing
	// `profile_id` lookup hits it.
	mt := L.NewTable()
	mt.RawSetString("__index", L.NewFunction(func(L *lua.LState) int {
		if L.CheckString(2) == "profile_id" {
			L.Push(lua.LString(scope.profileID))
			return 1
		}
		L.Push(lua.LNil)
		return 1
	}))
	L.SetMetatable(t, mt)

	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.image  —  analisi immagini lato server (luminanza logo, resize)
// ─────────────────────────────────────────────────────────────────────────────

func buildImageModule(L *lua.LState, opts SDKOpts) *lua.LTable {
	t := L.NewTable()

	// mycelium.image.analyze_logo(url) → {is_dark=bool, norm_w=int, norm_h=int}, err
	//
	// Scarica l'immagine, campiona la luminanza media dei pixel non trasparenti,
	// e calcola dimensioni normalizzate per rendere tutti i loghi confrontabili:
	//   - target width = 800px (proporzione mantenuta)
	//   - height cap = 200px (loghi quadrati ridotti)
	//   - se risulta più largo di 800px, scala da quella
	// is_dark=true quando luminanza media < 0.40 (logo scuro su sfondo trasparente).
	t.RawSetString("analyze_logo", L.NewFunction(func(L *lua.LState) int {
		url := L.CheckString(1)

		client := opts.HTTPClient
		if client == nil {
			client = http.DefaultClient
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")

		resp, err := client.Do(req)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		defer resp.Body.Close()

		imgData, _, err := image.Decode(io.LimitReader(resp.Body, maxLogoImageBytes))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("image decode: " + err.Error()))
			return 2
		}

		origW := imgData.Bounds().Dx()
		origH := imgData.Bounds().Dy()
		if origW == 0 || origH == 0 {
			L.Push(lua.LNil)
			L.Push(lua.LString("image has zero dimension"))
			return 2
		}

		// Dimensioni normalizzate: target 800px wide, cap 200px height.
		const targetW, maxH = 800, 200
		normW := targetW
		normH := (origH * targetW) / origW
		if normH > maxH {
			normH = maxH
			normW = (origW * maxH) / origH
		}

		// Campiona luminanza su thumbnail 32×32 per velocità.
		const sampleSize = 32
		sW := sampleSize
		sH := (origH * sampleSize) / origW
		if sH < 1 {
			sH = 1
		}

		thumb := image.NewNRGBA(image.Rect(0, 0, sW, sH))
		// Disegna l'immagine ridimensionata nel thumb (nearest-neighbour via draw).
		draw.Draw(thumb, thumb.Bounds(), image.NewUniform(image.Transparent), image.Point{}, draw.Src)
		srcBounds := imgData.Bounds()
		for ty := 0; ty < sH; ty++ {
			for tx := 0; tx < sW; tx++ {
				srcX := srcBounds.Min.X + (tx*origW)/sW
				srcY := srcBounds.Min.Y + (ty*origH)/sH
				thumb.Set(tx, ty, imgData.At(srcX, srcY))
			}
		}

		var totalLum float64
		var count int
		for ty := 0; ty < sH; ty++ {
			for tx := 0; tx < sW; tx++ {
				r, g, b, a := thumb.At(tx, ty).RGBA()
				// RGBA() returns 16-bit values (0–65535); alpha < 8192 ≈ < 12.5% opacity → skip
				if a < 8192 {
					continue
				}
				lum := (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)) / 65535.0
				totalLum += lum
				count++
			}
		}

		isDark := false
		if count > 0 {
			isDark = (totalLum / float64(count)) < 0.40
		}

		res := L.NewTable()
		res.RawSetString("is_dark", lua.LBool(isDark))
		res.RawSetString("norm_w", lua.LNumber(normW))
		res.RawSetString("norm_h", lua.LNumber(normH))
		L.Push(res)
		L.Push(lua.LNil)
		return 2
	}))

	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

func optStr(t *lua.LTable, key, def string) string {
	v := t.RawGetString(key)
	if s, ok := v.(lua.LString); ok {
		return string(s)
	}
	return def
}

func optInt(t *lua.LTable, key string, def int) int {
	v := t.RawGetString(key)
	if n, ok := v.(lua.LNumber); ok {
		return int(n)
	}
	return def
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.xml  (parser XML generico → lua table)
// ─────────────────────────────────────────────────────────────────────────────

func buildXMLModule(L *lua.LState) *lua.LTable {
	t := L.NewTable()

	// mycelium.xml.parse(xml_string) → table, err
	// Ogni nodo XML diventa una lua table con campi:
	//   _tag    : nome del tag
	//   _text   : testo diretto del nodo (trimmed)
	//   _attrs  : table con gli attributi (chiave→valore stringa)
	//   [1..N]  : figli (array)
	t.RawSetString("parse", L.NewFunction(func(L *lua.LState) int {
		xmlStr := L.CheckString(1)
		node, err := xmlToLua(L, []byte(xmlStr))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(node)
		L.Push(lua.LNil)
		return 2
	}))

	return t
}

// xmlToLua converte ricorsivamente un documento XML in una lua table.
func xmlToLua(L *lua.LState, data []byte) (*lua.LTable, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	// stack: ogni elemento è la table del nodo corrente
	var stack []*lua.LTable
	var root *lua.LTable

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			node := L.NewTable()
			node.RawSetString("_tag", lua.LString(t.Name.Local))
			node.RawSetString("_text", lua.LString(""))
			attrs := L.NewTable()
			for _, a := range t.Attr {
				attrs.RawSetString(a.Name.Local, lua.LString(a.Value))
			}
			node.RawSetString("_attrs", attrs)
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Append(node)
			}
			stack = append(stack, node)
			if root == nil {
				root = node
			}

		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}

		case xml.CharData:
			if len(stack) > 0 {
				text := strings.TrimSpace(string(t))
				if text != "" {
					cur := stack[len(stack)-1]
					existing := string(cur.RawGetString("_text").(lua.LString))
					if existing == "" {
						cur.RawSetString("_text", lua.LString(text))
					} else {
						cur.RawSetString("_text", lua.LString(existing+" "+text))
					}
				}
			}
		}
	}

	if root == nil {
		return L.NewTable(), nil
	}
	return root, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// mycelium.storage  —  lettura/scrittura file JSON nella directory del plugin
// ─────────────────────────────────────────────────────────────────────────────

func buildStorageModule(L *lua.LState, opts SDKOpts) *lua.LTable {
	t := L.NewTable()

	// Risolve un nome file nella dir del plugin. Blocca path traversal.
	resolvePath := func(name string) (string, error) {
		if opts.PluginDir == "" {
			return "", fmt.Errorf("storage: PluginDir not set")
		}
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return "", fmt.Errorf("storage: invalid path %q", name)
		}
		return filepath.Join(opts.PluginDir, clean), nil
	}

	// mycelium.storage.read_json(filename) → table|nil, err
	t.RawSetString("read_json", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		path, err := resolvePath(name)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				L.Push(lua.LNil)
				L.Push(lua.LNil)
			} else {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
			}
			return 2
		}
		var raw interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("json parse: " + err.Error()))
			return 2
		}
		lv := jsonToLua(L, raw)
		L.Push(lv)
		L.Push(lua.LNil)
		return 2
	}))

	// mycelium.storage.write_json(filename, table) → err|nil
	t.RawSetString("write_json", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		val := L.CheckAny(2)
		path, err := resolvePath(name)
		if err != nil {
			L.Push(lua.LString(err.Error()))
			return 1
		}
		native := luaToJSON(val)
		data, err := json.MarshalIndent(native, "", "  ")
		if err != nil {
			L.Push(lua.LString("json marshal: " + err.Error()))
			return 1
		}
		if len(data) > maxStorageWriteBytes {
			L.Push(lua.LString(fmt.Sprintf("storage.write_json: payload too large (%d bytes, max %d)", len(data), maxStorageWriteBytes)))
			return 1
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			L.Push(lua.LString(err.Error()))
			return 1
		}
		L.Push(lua.LNil)
		return 1
	}))

	// mycelium.storage.exists(filename) → bool
	t.RawSetString("exists", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		path, err := resolvePath(name)
		if err != nil {
			L.Push(lua.LFalse)
			return 1
		}
		_, err = os.Stat(path)
		L.Push(lua.LBool(err == nil))
		return 1
	}))

	return t
}

func jsonToLua(L *lua.LState, v interface{}) lua.LValue {
	if v == nil {
		return lua.LNil
	}
	switch val := v.(type) {
	case bool:
		return lua.LBool(val)
	case float64:
		return lua.LNumber(val)
	case string:
		return lua.LString(val)
	case []interface{}:
		tbl := L.NewTable()
		for i, item := range val {
			tbl.RawSetInt(i+1, jsonToLua(L, item))
		}
		return tbl
	case map[string]interface{}:
		tbl := L.NewTable()
		for k, item := range val {
			tbl.RawSetString(k, jsonToLua(L, item))
		}
		return tbl
	default:
		return lua.LString(fmt.Sprintf("%v", val))
	}
}

func luaToJSON(v lua.LValue) interface{} {
	switch val := v.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(val)
	case lua.LNumber:
		return float64(val)
	case lua.LString:
		return string(val)
	case *lua.LTable:
		// Determina se è array (chiavi 1..n) o oggetto
		isArray := true
		maxN := 0
		val.ForEach(func(k, _ lua.LValue) {
			if n, ok := k.(lua.LNumber); ok && float64(n) == float64(int(n)) && int(n) > 0 {
				if int(n) > maxN {
					maxN = int(n)
				}
			} else {
				isArray = false
			}
		})
		if isArray && maxN > 0 {
			arr := make([]interface{}, maxN)
			for i := 1; i <= maxN; i++ {
				arr[i-1] = luaToJSON(val.RawGetInt(i))
			}
			return arr
		}
		obj := make(map[string]interface{})
		val.ForEach(func(k, v lua.LValue) {
			obj[k.String()] = luaToJSON(v)
		})
		return obj
	default:
		return nil
	}
}
