package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/managers"
	"mycelium/internal/remote"
)

type fakeRemote struct {
	mu      sync.Mutex
	status  remote.Status
	running bool
	starts  []string
	stops   int
	logouts int
}

func (f *fakeRemote) Status() remote.Status { f.mu.Lock(); defer f.mu.Unlock(); return f.status }
func (f *fakeRemote) Running() bool         { f.mu.Lock(); defer f.mu.Unlock(); return f.running }
func (f *fakeRemote) Start(o remote.Options) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	f.starts = append(f.starts, o.Hostname+"|"+o.ControlURL)
	f.status = remote.Status{State: remote.StateStarting, Hostname: o.Hostname, ControlURL: o.ControlURL}
}
func (f *fakeRemote) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	f.stops++
	f.status = remote.Status{State: remote.StateDisabled}
}
func (f *fakeRemote) Logout(context.Context) error {
	f.Stop()
	f.mu.Lock()
	f.logouts++
	f.mu.Unlock()
	return nil
}
func (f *fakeRemote) Relogin(context.Context) error { return nil }
func (f *fakeRemote) IsTailnetHost(string) bool     { return false }

func setupRemoteTest(t *testing.T) *fakeRemote {
	t.Helper()
	oldBase := core.BasePath
	core.BasePath = t.TempDir()
	os.MkdirAll(filepath.Join(core.BasePath, "data"), 0o700)
	managers.Settings.Load()
	f := &fakeRemote{status: remote.Status{State: remote.StateDisabled}}
	setRemoteAccess(f)
	t.Cleanup(func() {
		setRemoteAccess(nil)
		core.BasePath = oldBase
		managers.Settings.Load()
	})
	return f
}

func remoteCall(t *testing.T, h http.HandlerFunc, method, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/admin/remote/x", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestRemoteEnableDisable(t *testing.T) {
	f := setupRemoteTest(t)

	_, st := remoteCall(t, getRemoteStatus, "GET", "")
	if st["state"] != "disabled" || st["enabled"] != false || st["available"] != true || st["hostname"] != "mycelium" {
		t.Fatalf("initial status %v", st)
	}

	if rec, _ := remoteCall(t, enableRemote, "POST", ""); rec.Code != 200 {
		t.Fatalf("enable: %d", rec.Code)
	}
	if !managers.Settings.GetBool(remoteEnabledKey, false) || len(f.starts) != 1 || f.starts[0] != "mycelium|" {
		t.Fatalf("enable not persisted/started: %v", f.starts)
	}

	// A running node is restarted with the new name; a bad one is refused.
	if rec, _ := remoteCall(t, saveRemoteSettings, "PUT", `{"hostname":"Bad Name"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad hostname: %d", rec.Code)
	}
	if rec, _ := remoteCall(t, saveRemoteSettings, "PUT", `{"control_url":"ftp://x"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad control url: %d", rec.Code)
	}
	if rec, _ := remoteCall(t, saveRemoteSettings, "PUT", `{"hostname":"casa","control_url":"https://hs.example.com/"}`); rec.Code != 200 {
		t.Fatalf("settings: %d", rec.Code)
	}
	if got := f.starts[len(f.starts)-1]; got != "casa|https://hs.example.com" {
		t.Fatalf("restart with %q", got)
	}

	// Logout keeps the feature on: a fresh node asks for a new login.
	if rec, _ := remoteCall(t, logoutRemote, "POST", ""); rec.Code != 200 || f.logouts != 1 || !f.running {
		t.Fatalf("logout: %d logouts=%d running=%v", rec.Code, f.logouts, f.running)
	}

	if rec, _ := remoteCall(t, disableRemote, "POST", ""); rec.Code != 200 {
		t.Fatalf("disable: %d", rec.Code)
	}
	if managers.Settings.GetBool(remoteEnabledKey, true) || f.running {
		t.Fatal("disable not persisted/stopped")
	}
	// Settings saved while stopped don't start anything.
	n := len(f.starts)
	remoteCall(t, saveRemoteSettings, "PUT", `{"hostname":"altro"}`)
	if len(f.starts) != n {
		t.Fatal("settings started a stopped node")
	}
}

func TestRemoteStatusURLs(t *testing.T) {
	f := setupRemoteTest(t)
	f.status = remote.Status{State: remote.StateRunning, FQDN: "mycelium.tail1234.ts.net", CertDomains: []string{"mycelium.tail1234.ts.net"}}

	_, st := remoteCall(t, getRemoteStatus, "GET", "")
	if _, ok := st["app_url"]; ok {
		t.Fatal("app URL handed out before the certificate is ready")
	}
	if st["console_url"] == nil {
		t.Fatal("no console link for Tailscale")
	}
	f.status.HTTPSReady = true
	_, st = remoteCall(t, getRemoteStatus, "GET", "")
	if st["app_url"] != "https://mycelium.tail1234.ts.net/app/" {
		t.Fatalf("app_url %v", st["app_url"])
	}

	req := httptest.NewRequest("GET", "/admin/remote/qr.svg?for=app", nil)
	rec := httptest.NewRecorder()
	remoteQR(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(rec.Body.String(), "<svg") {
		t.Fatalf("qr: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// No login link: nothing to encode.
	req = httptest.NewRequest("GET", "/admin/remote/qr.svg?for=login", nil)
	rec = httptest.NewRecorder()
	remoteQR(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("qr login without link: %d", rec.Code)
	}
}

func TestRemoteUnavailable(t *testing.T) {
	setupRemoteTest(t)
	setRemoteAccess(nil)
	if rec, _ := remoteCall(t, enableRemote, "POST", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("enable without manager: %d", rec.Code)
	}
	if _, st := remoteCall(t, getRemoteStatus, "GET", ""); st["available"] != false {
		t.Fatalf("status %v", st)
	}
}

// A Tailscale already on the machine is used as is: status, the address
// paired devices get, and tailnet clients' proxy URLs.
func TestRemoteExternalNode(t *testing.T) {
	f := setupRemoteTest(t)
	oldDetect := detectExternal
	ext := remote.External{Interface: "tailscale0", IPv4: "100.101.102.103", FQDN: "mycelium-fraffo.tail1234.ts.net"}
	detectExternal = func() (remote.External, bool) { return ext, true }
	t.Cleanup(func() { detectExternal = oldDetect })
	setRemoteAccess(f)

	_, st := remoteCall(t, getRemoteStatus, "GET", "")
	if st["state"] != "external" || st["app_url"] != "http://mycelium-fraffo.tail1234.ts.net:8000/app/" || st["ipv4"] != "100.101.102.103" {
		t.Fatalf("status %v", st)
	}
	// The node waiting for that Tailscale to stop shows the same.
	f.status = remote.Status{State: remote.StateHostTailscale, Err: "tailscale0"}
	if _, st := remoteCall(t, getRemoteStatus, "GET", ""); st["state"] != "external" || st["error"] != nil {
		t.Fatalf("waiting node: %v", st)
	}
	// Without its MagicDNS name: the IP.
	ext.FQDN = ""
	if _, st := remoteCall(t, getRemoteStatus, "GET", ""); st["app_url"] != "http://100.101.102.103:8000/app/" {
		t.Fatalf("by IP: %v", st["app_url"])
	}
	// Its own node, once running, wins.
	f.status = remote.Status{State: remote.StateRunning, IPv4: "100.64.0.9", FQDN: "mycelium.tail1234.ts.net", HTTPSReady: true}
	if _, st := remoteCall(t, getRemoteStatus, "GET", ""); st["state"] != "running" || st["app_url"] != "https://mycelium.tail1234.ts.net/app/" {
		t.Fatalf("own node: %v", st)
	}
	if host, _, ok := tailnetAddress(f); !ok || host != "100.64.0.9" {
		t.Fatalf("own node address %q", host)
	}
	f.status = remote.Status{State: remote.StateDisabled}
	if host, _, ok := tailnetAddress(f); !ok || host != "100.101.102.103" {
		t.Fatalf("external address %q", host)
	}

	// Installed but logged out: still the one to use, with no address yet.
	ext = remote.External{Interface: "tailscale0"}
	for _, s := range []remote.State{remote.StateDisabled, remote.StateHostTailscale} {
		f.status = remote.Status{State: s}
		if _, st := remoteCall(t, getRemoteStatus, "GET", ""); st["state"] != "external_offline" || st["app_url"] != nil {
			t.Fatalf("logged-out external (%s): %v", s, st)
		}
	}
	if _, _, ok := tailnetAddress(f); ok {
		t.Fatal("an address for a logged-out node")
	}
}
