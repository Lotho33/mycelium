//go:build !noremote

package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
)

func TestStateFromBackend(t *testing.T) {
	for in, want := range map[string]State{
		"NoState":          StateStarting,
		"Starting":         StateStarting,
		"NeedsLogin":       StateNeedsLogin,
		"Stopped":          StateNeedsLogin,
		"NeedsMachineAuth": StateNeedsApproval,
		"Running":          StateRunning,
		"InUseOtherUser":   StateError,
		"something-new":    StateStarting,
	} {
		if got := stateFromBackend(in); got != want {
			t.Errorf("stateFromBackend(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSnapshotRunning(t *testing.T) {
	exp := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	st := &ipnstate.Status{
		BackendState: "Running",
		AuthURL:      "https://login.example/stale",
		Self: &ipnstate.PeerStatus{
			DNSName:      "mycelium-1.tail1234.ts.net.",
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::1"), netip.MustParseAddr("100.101.102.103")},
			KeyExpiry:    &exp,
		},
		CurrentTailnet: &ipnstate.TailnetStatus{Name: "me@example.com", MagicDNSEnabled: true},
		CertDomains:    []string{"mycelium-1.tail1234.ts.net"},
	}
	prev := Status{State: StateRunning, Hostname: "mycelium", FQDN: "mycelium-1.tail1234.ts.net", HTTPSReady: true}
	s := snapshot(st, prev)
	if s.State != StateRunning || s.AuthURL != "" {
		t.Fatalf("state %s auth %q", s.State, s.AuthURL)
	}
	// The assigned name, not the requested one, without the trailing dot.
	if s.FQDN != "mycelium-1.tail1234.ts.net" || s.Hostname != "mycelium" {
		t.Fatalf("fqdn %q hostname %q", s.FQDN, s.Hostname)
	}
	if s.IPv4 != "100.101.102.103" || s.IPv6 != "fd7a:115c:a1e0::1" {
		t.Fatalf("ips %q %q", s.IPv4, s.IPv6)
	}
	if s.KeyExpiry == nil || !s.KeyExpiry.Equal(exp) || !s.MagicDNS || s.Tailnet != "me@example.com" {
		t.Fatalf("expiry/tailnet: %+v", s)
	}
	if !s.HTTPSReady {
		t.Fatal("certificate readiness lost for the same name")
	}
	// A new name needs a new certificate.
	st.Self.DNSName = "mycelium-2.tail1234.ts.net."
	if snapshot(st, prev).HTTPSReady {
		t.Fatal("certificate readiness carried over to another name")
	}
}

func TestSnapshotNeedsLogin(t *testing.T) {
	st := &ipnstate.Status{BackendState: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/abc"}
	s := snapshot(st, Status{Hostname: "mycelium"})
	if s.State != StateNeedsLogin || s.AuthURL != "https://login.tailscale.com/a/abc" || s.FQDN != "" {
		t.Fatalf("%+v", s)
	}
}

func TestValidHostname(t *testing.T) {
	for h, want := range map[string]bool{
		"mycelium": true, "mycelium-2": true, "a": true, "": false, "-x": false, "x-": false,
		"My": false, "a.b": false, "a_b": false, string(make([]byte, 64)): false,
	} {
		if got := ValidHostname(h); got != want {
			t.Errorf("ValidHostname(%q) = %v", h, got)
		}
	}
}

func TestIsTailnetHost(t *testing.T) {
	m := New(Config{})
	m.status = Status{State: StateRunning, FQDN: "mycelium.tail1234.ts.net", IPv4: "100.101.102.103", IPv6: "fd7a:115c:a1e0::1"}
	for h, want := range map[string]bool{
		"mycelium.tail1234.ts.net":     true,
		"MYCELIUM.tail1234.ts.net.":    true,
		"mycelium.tail1234.ts.net:443": true,
		"100.101.102.103:50051":        true,
		"[fd7a:115c:a1e0::1]:8000":     true,
		"192.168.1.10:8000":            false,
		"other.tail1234.ts.net":        false,
		"":                             false,
	} {
		if got := m.IsTailnetHost(h); got != want {
			t.Errorf("IsTailnetHost(%q) = %v", h, got)
		}
	}
	m.status.State = StateNeedsLogin
	if m.IsTailnetHost("mycelium.tail1234.ts.net") {
		t.Error("a node that isn't running names nothing")
	}
}

func TestRedirectHandler(t *testing.T) {
	m := New(Config{})
	m.status = Status{State: StateRunning, FQDN: "mycelium.tail1234.ts.net"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	h := m.redirectHandler(next)

	do := func(host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://"+host+"/app/?x=1", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// No certificate yet: served as is.
	if rec := do("mycelium.tail1234.ts.net"); rec.Code != 299 {
		t.Fatalf("before HTTPS: %d", rec.Code)
	}
	m.status.HTTPSReady = true
	rec := do("mycelium.tail1234.ts.net")
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://mycelium.tail1234.ts.net/app/?x=1" {
		t.Fatalf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// By IP (native clients): no redirect, the certificate has no IP.
	if rec := do("100.101.102.103"); rec.Code != 299 {
		t.Fatalf("by IP: %d", rec.Code)
	}
}

// The node must never be published on the Internet: no Funnel listener.
func TestNoFunnel(t *testing.T) {
	src, err := os.ReadFile("tsnet.go")
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`(?m)//.*$`).ReplaceAll(src, nil)
	if regexp.MustCompile(`ListenFunnel|FunnelOnly|FunnelTLSConfig`).Match(code) {
		t.Fatal("tsnet.go uses Funnel")
	}
}

func TestStopWithoutStart(t *testing.T) {
	m := New(Config{Hostname: "mycelium"})
	m.Stop()
	if s := m.Status(); s.State != StateDisabled || m.Running() {
		t.Fatalf("%+v", s)
	}
}

// Another Tailscale on the machine: the node waits, then starts once it's
// gone (here it is stopped while still waiting).
func TestWaitHostTailscale(t *testing.T) {
	present := true
	var mu sync.Mutex
	oldFn, oldRecheck := hostTailscale, hostTailscaleRecheck
	hostTailscale = func() string {
		mu.Lock()
		defer mu.Unlock()
		if present {
			return "tailscale0"
		}
		return ""
	}
	hostTailscaleRecheck = 10 * time.Millisecond
	t.Cleanup(func() { hostTailscale, hostTailscaleRecheck = oldFn, oldRecheck })

	m := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan bool, 1)
	go func() { res <- m.waitHostTailscale(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for m.Status().State != StateHostTailscale {
		if time.Now().After(deadline) {
			t.Fatal("never reported the other Tailscale")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s := m.Status(); s.Err != "tailscale0" {
		t.Fatalf("interface not reported: %+v", s)
	}
	mu.Lock()
	present = false
	mu.Unlock()
	select {
	case ok := <-res:
		if !ok {
			t.Fatal("wait reported a cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("didn't go on once the other Tailscale was gone")
	}

	// Stopped while waiting: no start.
	mu.Lock()
	present = true
	mu.Unlock()
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { res <- m.waitHostTailscale(ctx2) }()
	time.Sleep(30 * time.Millisecond)
	cancel2()
	if ok := <-res; ok {
		t.Fatal("started after being stopped")
	}
}
