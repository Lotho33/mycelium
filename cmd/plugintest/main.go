// plugintest runs a plugin's own Lua test files against the real
// mycelium.* SDK, with the network answered from canned routes — so a
// plugin can be checked without installing it on a server or reaching its
// sources. It knows nothing about any plugin: a test file declares the
// routes it needs and calls the plugin's entrypoints.
//
//	go run ./cmd/plugintest -plugin ./myplugin [-set key=value ...] [-v] test1.lua [test2.lua ...]
//
// -set sets one of the plugin's global settings (mycelium.context.
// get_global_setting), e.g. an API token the code expects to find.
//
// In a test file, besides the plugin's globals:
//
//	plugintest.route(pattern, response)  -- pattern: Go regexp on the URL;
//	    response: a body string, or {status=, body=, headers={...}}.
//	    The most recently added matching route answers; an unmatched
//	    request gets status 599 and is reported.
//	plugintest.requests()  -- the requests made so far: {method=, url=, headers=}
//	plugintest.clear()     -- forget the recorded requests
//	check(cond, message)   -- fails the file when cond is false
//
// Each file runs in a fresh state (the plugin loaded again).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	lua "github.com/yuin/gopher-lua"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

type route struct {
	re      *regexp.Regexp
	status  int
	body    string
	headers map[string]string
}

type recorded struct {
	method, url string
	headers     http.Header
}

// fakeNet is the http.RoundTripper behind every SDK call. mycelium.network.
// get_many runs requests in parallel, hence the lock.
type fakeNet struct {
	mu        sync.Mutex
	routes    []route
	reqs      []recorded
	unmatched []string
}

func (f *fakeNet) RoundTrip(r *http.Request) (*http.Response, error) {
	u := r.URL.String()
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{r.Method, u, r.Header.Clone()})
	var hit *route
	for i := len(f.routes) - 1; i >= 0; i-- {
		if f.routes[i].re.MatchString(u) {
			hit = &f.routes[i]
			break
		}
	}
	if hit == nil {
		f.unmatched = append(f.unmatched, r.Method+" "+u)
	}
	f.mu.Unlock()
	resp := &http.Response{
		StatusCode: 599,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("plugintest: no route for " + u)),
		Request:    r,
	}
	if hit != nil {
		resp.StatusCode = hit.status
		resp.Body = io.NopCloser(bytes.NewReader([]byte(hit.body)))
		for k, v := range hit.headers {
			resp.Header.Set(k, v)
		}
	}
	resp.Status = fmt.Sprintf("%d", resp.StatusCode)
	return resp, nil
}

type settingsFlag []string

func (s *settingsFlag) String() string     { return strings.Join(*s, ",") }
func (s *settingsFlag) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	dir := flag.String("plugin", "", "plugin directory (manifest.yaml + init.lua)")
	verbose := flag.Bool("v", false, "show the plugin's own log lines")
	var sets settingsFlag
	flag.Var(&sets, "set", "plugin global setting key=value (repeatable)")
	flag.Parse()
	if *dir == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "uso: plugintest -plugin <dir> [-set k=v] [-v] test.lua ...")
		os.Exit(2)
	}
	if !*verbose {
		log.SetOutput(io.Discard)
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	failed := 0
	for _, file := range flag.Args() {
		if err := runFile(abs, file, sets); err != nil {
			fmt.Printf("FAIL %s\n  %v\n", file, err)
			failed++
		} else {
			fmt.Printf("PASS %s\n", file)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func runFile(dir, file string, sets []string) error {
	net := &fakeNet{}
	L, id, err := engine.NewPluginTestState(dir, &http.Client{Transport: net})
	if err != nil {
		return fmt.Errorf("caricamento del plugin: %w", err)
	}
	defer L.Close()
	for _, kv := range sets {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("-set %q: serve key=value", kv)
		}
		managers.Settings.SetInMemory("lua:"+id+":global:"+k, v)
	}
	installAPI(L, net)
	src, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	// The plugin's state has a locked-down stdlib (no dofile/loadfile): the
	// test runs as a string chunk in it.
	fn, err := L.LoadString(string(src))
	if err != nil {
		return err
	}
	L.Push(fn)
	runErr := L.PCall(0, 0, nil)
	// Listed even when the file failed: a missing route is often why.
	if len(net.unmatched) > 0 {
		missing := fmt.Errorf("richieste senza route:\n    %s", strings.Join(net.unmatched, "\n    "))
		if runErr != nil {
			return fmt.Errorf("%w\n  %v", runErr, missing)
		}
		return missing
	}
	return runErr
}

func installAPI(L *lua.LState, net *fakeNet) {
	t := L.NewTable()
	t.RawSetString("route", L.NewFunction(func(L *lua.LState) int {
		re, err := regexp.Compile(L.CheckString(1))
		if err != nil {
			L.ArgError(1, err.Error())
		}
		r := route{re: re, status: 200}
		switch v := L.Get(2).(type) {
		case lua.LString:
			r.body = string(v)
		case *lua.LTable:
			if s, ok := v.RawGetString("status").(lua.LNumber); ok {
				r.status = int(s)
			}
			r.body = lua.LVAsString(v.RawGetString("body"))
			if h, ok := v.RawGetString("headers").(*lua.LTable); ok {
				r.headers = map[string]string{}
				h.ForEach(func(k, val lua.LValue) { r.headers[k.String()] = val.String() })
			}
		default:
			L.ArgError(2, "body string or {status=, body=, headers=}")
		}
		net.mu.Lock()
		net.routes = append(net.routes, r)
		net.mu.Unlock()
		return 0
	}))
	t.RawSetString("requests", L.NewFunction(func(L *lua.LState) int {
		out := L.NewTable()
		net.mu.Lock()
		defer net.mu.Unlock()
		for _, r := range net.reqs {
			e := L.NewTable()
			e.RawSetString("method", lua.LString(r.method))
			e.RawSetString("url", lua.LString(r.url))
			h := L.NewTable()
			for k := range r.headers {
				h.RawSetString(k, lua.LString(r.headers.Get(k)))
			}
			e.RawSetString("headers", h)
			out.Append(e)
		}
		L.Push(out)
		return 1
	}))
	t.RawSetString("clear", L.NewFunction(func(L *lua.LState) int {
		net.mu.Lock()
		net.reqs = nil
		net.mu.Unlock()
		return 0
	}))
	L.SetGlobal("plugintest", t)
	L.SetGlobal("check", L.NewFunction(func(L *lua.LState) int {
		if !lua.LVAsBool(L.Get(1)) {
			L.RaiseError("check fallito: %s", L.OptString(2, ""))
		}
		return 0
	}))
}
