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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"

	"github.com/Lotho33/stipes-sdk/sdk/gen"
	"github.com/PuerkitoBio/goquery"
	"github.com/tidwall/gjson"
	lua "github.com/yuin/gopher-lua"
)

// ProgressFunc receives a status ("", "success", "warning", "error") and a
// free-form, plugin-owned message from mycelium.progress() Lua calls.
type ProgressFunc func(status, message string)

// browserAPI is what the Lua SDK needs from the optional browser service,
// implemented by *managers.BrowserServiceClient.
type browserAPI interface {
	NavigateVia(ctx context.Context, req *gen.NavigateRequest, proxyURL string) (*gen.NavigateResponse, error)
	EvalVia(ctx context.Context, req *gen.EvalRequest, proxyURL string) (*gen.EvalResponse, error)
	WaitInterceptVia(ctx context.Context, req *gen.WaitInterceptRequest, proxyURL string) (*gen.WaitInterceptResponse, error)
}

// currentBrowserClient returns managers.BrowserClient as a browserAPI, or a
// true nil interface when unset (a typed nil pointer would defeat the
// "not available" check).
func currentBrowserClient() browserAPI {
	if managers.BrowserClient == nil {
		return nil
	}
	return managers.BrowserClient
}

// maxSleepMillis caps mycelium.sleep(ms) around the entrypoint budget.
const maxSleepMillis = int(defaultEntrypointTimeout / time.Millisecond)

// maxSDKResponseBytes caps how much of an HTTP response body
// mycelium.network.get/post/fetch reads into memory.
const maxSDKResponseBytes = 32 << 20 // 32 MiB

// maxLogoImageBytes caps the source image mycelium.image.analyze_logo reads.
const maxLogoImageBytes = 8 << 20 // 8 MiB

// maxLogoImagePixels caps the logo's declared dimensions (checked with
// image.DecodeConfig before decoding).
const maxLogoImagePixels = 16_000_000

// maxCacheValueBytes caps a single mycelium.cache.set value, so a plugin
// can't grow the shared Redis without bound.
const maxCacheValueBytes = 4 << 20 // 4 MiB

// maxStorageWriteBytes caps a single mycelium.storage.write_json payload.
const maxStorageWriteBytes = 16 << 20 // 16 MiB

// SDKOpts carries the per-plugin invariants of the Lua SDK, set once when a
// pooled LState is built. Per-call state (profile, proxy requirement,
// progress sink) lives in callScope (lua_scope.go).
type SDKOpts struct {
	PluginID        string
	PluginDir       string
	Redis           *managers.RedisRepo
	Browser         browserAPI
	LogBuf          *managers.PluginLogBuffer
	HTTPClient      *http.Client // direct client (no proxy)
	ProxyHTTPClient *http.Client // single-proxy client, used when ProxyClientFor is unset
	// ProxyURL is the single VPN/proxy URL (e.g. socks5://proxy:1080).
	ProxyURL string
	// ProxyClientFor returns the http.Client for an egress proxy URL (the
	// call's selected egress). nil-safe: falls back to ProxyHTTPClient.
	ProxyClientFor func(proxyURL string) *http.Client
}

// scopeProxyClient picks the http.Client of a proxied call. A requireProxy
// call with an empty proxyURL means the selected egress is disabled: it
// returns nil so the SDK blocks the call instead of falling back.
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

// RegisterSDK registers the mycelium.* modules on L, once per pooled LState;
// the closures read the current call from scope.
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

	// progress(status, message) — an update while the plugin is working (e.g.
	// in resolve_stream). status: "", "success", "warning", "error". No-op when
	// the call doesn't stream progress.
	mycelium.RawSetString("progress", L.NewFunction(func(L *lua.LState) int {
		statusStr := L.OptString(1, "")
		msg := L.OptString(2, "")
		if scope.onProgress != nil {
			scope.onProgress(statusStr, msg)
		}
		return 0
	}))

	// mycelium.sleep(ms) — capped at maxSleepMillis and cut short when the
	// call's context is done, so an abandoned call doesn't linger.
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
		ctx, cancel := context.WithTimeout(scope.parentCtx(), clampSDKTimeout(timeoutSec))
		defer cancel()

		// Built once, applied to each attempt's request.
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

		// Client selection:
		//   - VPN-routed plugins (the default): always through the proxy, fail
		//     closed without one; the _proxy header can't opt out.
		//   - direct_egress plugins: _proxy=true uses the proxy when available.
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

		// Transport-level failures (proxy hiccup, DNS blip) are retried a couple of
		// times; HTTP responses, error statuses included, are returned as they are.
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
				log.Printf("[sdk/network] %s %s → error (attempt %d/%d): %v", method, sdkLogURL(rawURL), attempt, maxAttempts, core.RedactURLError(err))
				time.Sleep(time.Duration(attempt) * 150 * time.Millisecond)
				continue
			}
		}
		if err != nil {
			log.Printf("[sdk/network] %s %s → error: %v", method, sdkLogURL(rawURL), core.RedactURLError(err))
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
				// Join multiple values with \n so Lua can split them (e.g. Set-Cookie).
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

	// mycelium.network.fetch(url [, opts_table]) → body_string, err_string
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

	// mycelium.dom.select(html, css) → array of {text, html, attr=fn}
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

	// mycelium.json.get(json_string, path) → value (gjson path)
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

	// mycelium.json.get_array(json_string, path) → lua array of raw strings
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

func buildBrowserModule(L *lua.LState, opts SDKOpts, scope *callScope) *lua.LTable {
	t := L.NewTable()
	bm := opts.Browser

	// VPN-routed calls send their browser traffic through the VPN too, decided
	// per call from callScope. blocked means "must use a proxy, none available".
	proxyFor := func() (proxyURL string, blocked bool) {
		if !scope.requireProxy {
			return "", false
		}
		if scope.proxyURL == "" {
			return "", true // selected egress unavailable: block
		}
		return scope.proxyURL, false
	}

	// mycelium.browser.sniff(trigger_url, url_pattern, timeout_seconds) → intercepted_url, headers, err
	// headers holds the intercepted request's headers ({[name]=value}).
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
		ctx, cancel := context.WithTimeout(scope.parentCtx(), clampSDKTimeout(timeoutSec))
		defer cancel()
		resp, err := bm.WaitInterceptVia(ctx, &gen.WaitInterceptRequest{
			TriggerUrl: triggerURL,
			UrlPattern: pattern,
			TimeoutMs:  int64(timeoutSec) * 1000,
		}, proxyURL)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 3
		}
		hdrs := L.NewTable()
		hdrKeys := make([]string, 0, len(resp.Headers))
		for k, v := range resp.Headers {
			hdrs.RawSetString(k, lua.LString(v))
			hdrKeys = append(hdrKeys, k)
		}
		// Log the header names returned (values elided).
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
		ctx, cancel := context.WithTimeout(scope.parentCtx(), clampSDKTimeout(timeoutSec))
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
		ctx, cancel := context.WithTimeout(scope.parentCtx(), clampSDKTimeout(timeoutSec))
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
// mycelium.context  (per-profile secrets, stored in SQLite)
// ─────────────────────────────────────────────────────────────────────────────

func buildContextModule(L *lua.LState, opts SDKOpts, scope *callScope) *lua.LTable {
	t := L.NewTable()

	// Secrets live in SQLite; a value found only in the legacy Redis hash is
	// migrated on first read.
	t.RawSetString("get_secret", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		if scope.profileID == "" {
			L.Push(lua.LString(""))
			return 1
		}
		if managers.DB != nil {
			if v, ok, err := managers.DB.GetPluginSecret(opts.PluginID, scope.profileID, key); err == nil && ok {
				L.Push(lua.LString(v))
				return 1
			}
		}
		val := ""
		if opts.Redis != nil {
			val, _ = opts.Redis.HGet(context.Background(), managers.SecretsKey(opts.PluginID, scope.profileID), key)
			if val != "" && managers.DB != nil {
				_ = managers.DB.SetPluginSecret(opts.PluginID, scope.profileID, key, val)
			}
		}
		L.Push(lua.LString(val))
		return 1
	}))

	// mycelium.context.set_secret(key, value) → true | false, err
	t.RawSetString("set_secret", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		val := L.CheckString(2)
		if scope.profileID == "" {
			L.Push(lua.LFalse)
			L.Push(lua.LString("set_secret: nessun profilo attivo"))
			return 2
		}
		if managers.DB == nil {
			L.Push(lua.LFalse)
			L.Push(lua.LString("set_secret: database non disponibile"))
			return 2
		}
		if err := managers.DB.SetPluginSecret(opts.PluginID, scope.profileID, key, val); err != nil {
			L.Push(lua.LFalse)
			L.Push(lua.LString("set_secret: " + err.Error()))
			return 2
		}
		L.Push(lua.LTrue)
		return 1
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
	// label: "ready"|"syncing"|"needs_config"|"error"; detail: free text ("" clears)
	t.RawSetString("set_plugin_status", L.NewFunction(func(L *lua.LState) int {
		label := L.CheckString(1)
		detail := L.OptString(2, "")
		LuaPlugins.SetStatus(opts.PluginID, label, detail)
		return 0
	}))

	t.RawSetString("plugin_id", lua.LString(opts.PluginID))

	// mycelium.context.profile_id — kept live through an __index metatable
	// (the table is built once).
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
// mycelium.image — server-side image analysis
// ─────────────────────────────────────────────────────────────────────────────

func buildImageModule(L *lua.LState, opts SDKOpts) *lua.LTable {
	t := L.NewTable()

	// mycelium.image.analyze_logo(url) → {is_dark=bool, norm_w=int, norm_h=int}, err
	//
	// Samples the mean luminance of the non-transparent pixels and computes a
	// normalized size (800px wide, height capped at 200px) so logos render at
	// comparable sizes. is_dark when the mean luminance is below 0.40.
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

		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLogoImageBytes))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		// Check the declared size first (decompression bomb).
		if cfg, _, cerr := image.DecodeConfig(bytes.NewReader(raw)); cerr == nil &&
			int64(cfg.Width)*int64(cfg.Height) > maxLogoImagePixels {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("image too large: %dx%d", cfg.Width, cfg.Height)))
			return 2
		}
		imgData, _, err := image.Decode(bytes.NewReader(raw))
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

		// Normalized size: 800px wide, height capped at 200px.
		const targetW, maxH = 800, 200
		normW := targetW
		normH := (origH * targetW) / origW
		if normH > maxH {
			normH = maxH
			normW = (origW * maxH) / origH
		}

		// Sample luminance on a 32×32 thumbnail.
		const sampleSize = 32
		sW := sampleSize
		sH := (origH * sampleSize) / origW
		if sH < 1 {
			sH = 1
		}

		thumb := image.NewNRGBA(image.Rect(0, 0, sW, sH))
		// Draw the image scaled into the thumbnail (nearest-neighbour).
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
				// RGBA() is 16-bit; alpha < 8192 (≈12.5% opacity) is skipped
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
// mycelium.xml — generic XML → Lua table
// ─────────────────────────────────────────────────────────────────────────────

func buildXMLModule(L *lua.LState) *lua.LTable {
	t := L.NewTable()

	// mycelium.xml.parse(xml_string) → table, err
	// Each node becomes a table with:
	//   _tag    : tag name
	//   _text   : the node's own text (trimmed)
	//   _attrs  : attributes (name → string)
	//   [1..N]  : children
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

// xmlToLua converts an XML document into a Lua table.
func xmlToLua(L *lua.LState, data []byte) (*lua.LTable, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	// stack of the open nodes' tables
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
// mycelium.storage — JSON files in the plugin's directory
// ─────────────────────────────────────────────────────────────────────────────

func buildStorageModule(L *lua.LState, opts SDKOpts) *lua.LTable {
	t := L.NewTable()

	// Resolves a file name inside the plugin directory, rejecting traversal.
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
		// Only *.json data files: a plugin must not rewrite its own code or
		// manifest.yaml (it could grant itself direct_egress or another id).
		if !strings.EqualFold(filepath.Ext(path), ".json") {
			L.Push(lua.LString(fmt.Sprintf("storage.write_json: only .json files may be written (%q)", name)))
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
	return luaToJSONGuarded(v, newLuaConvGuard())
}

func luaToJSONGuarded(v lua.LValue, g *luaConvGuard) interface{} {
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
		if !g.enter(val) {
			return nil // cycle / too deep / too many tables
		}
		defer g.leave(val)
		// Array (keys 1..n) or object?
		isArray := true
		maxN, count := 0, 0
		val.ForEach(func(k, _ lua.LValue) {
			count++
			if n, ok := k.(lua.LNumber); ok && float64(n) == float64(int(n)) && int(n) > 0 {
				if int(n) > maxN {
					maxN = int(n)
				}
			} else {
				isArray = false
			}
		})
		// A few holes still make an array, but a sparse t[1e9] = 1 must not
		// allocate a billion-slot slice.
		if isArray && maxN > 0 && maxN <= 2*count {
			arr := make([]interface{}, maxN)
			for i := 1; i <= maxN; i++ {
				arr[i-1] = luaToJSONGuarded(val.RawGetInt(i), g)
			}
			return arr
		}
		obj := make(map[string]interface{})
		val.ForEach(func(k, v lua.LValue) {
			obj[k.String()] = luaToJSONGuarded(v, g)
		})
		return obj
	default:
		return nil
	}
}

// sdkLogURL is rawURL without its query (plugin API keys, tokens) for logs.
func sdkLogURL(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	return "(url)"
}
