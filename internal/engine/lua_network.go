package engine

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"

	lua "github.com/yuin/gopher-lua"
)

// sdkRequest is one mycelium.network.* request, already read out of Lua so
// it can run on any goroutine.
type sdkRequest struct {
	method, url, body string
	headers           map[string]string
	optInProxy        bool // _proxy header (direct_egress plugins only)
	timeout           time.Duration
}

// sdkResponse is a finished request; toLua builds the resp_table.
type sdkResponse struct {
	status  int
	body    string
	headers http.Header
	err     string
}

// sdkRequestFromLua reads a request; the _proxy pseudo-header selects the
// proxy instead of being sent.
func sdkRequestFromLua(method, rawURL, body string, headers *lua.LTable, timeoutSec int) sdkRequest {
	r := sdkRequest{method: method, url: rawURL, body: body, headers: map[string]string{}, timeout: clampSDKTimeout(timeoutSec)}
	if headers != nil {
		headers.ForEach(func(k, v lua.LValue) {
			key := k.String()
			if key == "_proxy" {
				r.optInProxy = lua.LVAsBool(v)
				return
			}
			r.headers[key] = v.String()
		})
	}
	return r
}

func (r sdkResponse) toLua(L *lua.LState) *lua.LTable {
	res := L.NewTable()
	res.RawSetString("status_code", lua.LNumber(r.status))
	res.RawSetString("body", lua.LString(r.body))
	if r.err != "" {
		res.RawSetString("error", lua.LString(r.err))
		return res
	}
	respHeaders := L.NewTable()
	for k, vs := range r.headers {
		if len(vs) > 0 {
			// Join multiple values with \n so Lua can split them (e.g. Set-Cookie).
			respHeaders.RawSetString(k, lua.LString(strings.Join(vs, "\n")))
		}
	}
	res.RawSetString("headers", respHeaders)
	res.RawSetString("error", lua.LNil)
	return res
}

// doSDKHTTP performs one request for the plugin, reading at most maxBody
// bytes of the response. It doesn't touch the LState, so get_many runs it on
// several goroutines.
func (opts SDKOpts) doSDKHTTP(scope *callScope, r sdkRequest, maxBody int64) sdkResponse {
	ctx, cancel := context.WithTimeout(scope.parentCtx(), r.timeout)
	defer cancel()

	// Client selection:
	//   - VPN-routed plugins (the default): always through the proxy, fail
	//     closed without one; the _proxy header can't opt out.
	//   - direct_egress plugins: _proxy=true uses the proxy when available.
	client := opts.HTTPClient
	if scope.requireProxy {
		pc := opts.scopeProxyClient(scope)
		if pc == nil {
			return sdkResponse{err: "plugin requires VPN but the selected egress is unavailable"}
		}
		client = pc
	} else if r.optInProxy {
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
		if r.body != "" {
			bodyReader = strings.NewReader(r.body)
		}
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, r.method, r.url, bodyReader)
		if err != nil {
			return sdkResponse{err: err.Error()}
		}
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", defaultPluginUserAgent)
		}

		resp, err = client.Do(req)
		if err == nil {
			break
		}
		if attempt < maxAttempts && ctx.Err() == nil {
			log.Printf("[sdk/network] %s %s → error (attempt %d/%d): %v", r.method, sdkLogURL(r.url), attempt, maxAttempts, core.RedactURLError(err))
			time.Sleep(time.Duration(attempt) * 150 * time.Millisecond)
			continue
		}
	}
	if err != nil {
		log.Printf("[sdk/network] %s %s → error: %v", r.method, sdkLogURL(r.url), core.RedactURLError(err))
		return sdkResponse{err: err.Error()}
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, io.LimitReader(resp.Body, maxBody))
	return sdkResponse{status: resp.StatusCode, body: buf.String(), headers: resp.Header}
}

const (
	maxGetManyRequests    = 32
	defaultGetManyWorkers = 6
	maxGetManyWorkers     = 16
	// maxGetManyBody caps each response of a batch (lower than a single
	// request's cap: the whole batch is held in memory at once).
	maxGetManyBody = 8 << 20
)

// networkGetMany implements mycelium.network.get_many. Requests are read from
// Lua up front, run on at most `concurrency` goroutines, and converted back
// on the calling goroutine: the LState is never touched concurrently.
func networkGetMany(L *lua.LState, opts SDKOpts, scope *callScope, list, o *lua.LTable) *lua.LTable {
	workers, defTimeout, limiter := defaultGetManyWorkers, 30, ""
	if o != nil {
		workers = optInt(o, "concurrency", workers)
		defTimeout = optInt(o, "timeout", defTimeout)
		limiter = optStr(o, "ratelimit", "")
	}
	workers = max(1, min(workers, maxGetManyWorkers))

	n := list.Len()
	if n > maxGetManyRequests {
		L.RaiseError("network.get_many: too many requests (%d, max %d)", n, maxGetManyRequests)
	}
	reqs := make([]sdkRequest, n)
	for i := 0; i < n; i++ {
		switch v := list.RawGetInt(i + 1).(type) {
		case lua.LString:
			reqs[i] = sdkRequestFromLua("GET", string(v), "", nil, defTimeout)
		case *lua.LTable:
			var hdrs *lua.LTable
			if h, ok := v.RawGetString("headers").(*lua.LTable); ok {
				hdrs = h
			}
			reqs[i] = sdkRequestFromLua(strings.ToUpper(optStr(v, "method", "GET")), optStr(v, "url", ""),
				optStr(v, "body", ""), hdrs, optInt(v, "timeout", defTimeout))
		default:
			L.ArgError(1, "each request must be a URL or a table")
		}
	}

	resps := make([]sdkResponse, n)
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(workers, n); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				// Guard per request: a recovered panic must not stop the
				// worker, or the requests still queued would never be taken.
				func() {
					defer core.Guard("lua/get_many:" + opts.PluginID)
					resps[i] = sdkResponse{err: "request failed"}
					if limiter != "" && !pluginLimiters.wait(scope.parentCtx(), opts.PluginID, limiter, reqs[i].timeout) {
						resps[i] = sdkResponse{err: "ratelimit " + limiter + ": wait cancelled"}
						return
					}
					resps[i] = opts.doSDKHTTP(scope, reqs[i], maxGetManyBody)
				}()
			}
		}()
	}
	for i := range reqs {
		if reqs[i].url == "" {
			resps[i] = sdkResponse{err: "missing url"}
			continue
		}
		next <- i
	}
	close(next)
	wg.Wait()

	out := L.CreateTable(n, 0)
	for _, r := range resps {
		out.Append(r.toLua(L))
	}
	return out
}
