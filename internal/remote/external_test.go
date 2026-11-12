package remote

import (
	"testing"
	"time"
)

func fakeInterfaces(t *testing.T, ifs []ifaceInfo, ptr func(string) string) {
	t.Helper()
	oldIfs, oldPTR := interfacesFn, lookupPTR
	interfacesFn = func() ([]ifaceInfo, error) { return ifs, nil }
	lookupPTR = ptr
	resetExternal := func() {
		ext.Lock()
		ext.at, ext.ptrIP, ext.ptrName, ext.ptrBusy = time.Time{}, "", "", false
		ext.Unlock()
	}
	resetExternal()
	t.Cleanup(func() {
		interfacesFn, lookupPTR = oldIfs, oldPTR
		resetExternal()
	})
}

func TestDetectExternal(t *testing.T) {
	cases := []struct {
		name string
		ifs  []ifaceInfo
		want External
		ok   bool
	}{
		{"none", []ifaceInfo{{Name: "lo", Up: true, Addrs: []string{"127.0.0.1/8"}}, {Name: "eth0", Up: true, Addrs: []string{"192.168.1.10/24"}}}, External{}, false},
		{"ISP CGNAT on the LAN card is not tailscale", []ifaceInfo{{Name: "eth0", Up: true, Addrs: []string{"100.72.1.2/10"}}}, External{}, false},
		{"tailscale0", []ifaceInfo{{Name: "eth0", Up: true, Addrs: []string{"192.168.1.10/24"}}, {Name: "tailscale0", Up: true, Addrs: []string{"100.101.102.103/32", "fd7a:115c:a1e0::5/128"}}},
			External{Interface: "tailscale0", IPv4: "100.101.102.103", IPv6: "fd7a:115c:a1e0::5"}, true},
		{"renamed TUN, found by its tailscale IPv6", []ifaceInfo{{Name: "ts9", Up: true, Addrs: []string{"100.101.102.104/32", "fd7a:115c:a1e0::6/128"}}},
			External{Interface: "ts9", IPv4: "100.101.102.104", IPv6: "fd7a:115c:a1e0::6"}, true},
		{"logged-out tailscaled: interface without a tailnet address", []ifaceInfo{{Name: "tailscale0", Up: true, Addrs: []string{"fe80::59e4:f3e0:51e7:5535/64"}}}, External{}, false},
		{"down", []ifaceInfo{{Name: "tailscale0", Up: false, Addrs: []string{"100.101.102.103/32"}}}, External{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeInterfaces(t, c.ifs, func(string) string { return "" })
			got, ok := DetectExternal()
			if ok != c.ok || got != c.want {
				t.Fatalf("got %+v %v, want %+v %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestExternalMagicDNSName(t *testing.T) {
	asked := make(chan string, 4)
	fakeInterfaces(t, []ifaceInfo{{Name: "tailscale0", Up: true, Addrs: []string{"100.101.102.103/32"}}},
		func(ip string) string { asked <- ip; return "mycelium-fraffo.tail1234.ts.net" })

	e, ok := DetectExternal()
	if !ok || e.FQDN != "" || e.Host() != "100.101.102.103" {
		t.Fatalf("first detection: %+v %v", e, ok)
	}
	if ip := <-asked; ip != "100.101.102.103" {
		t.Fatalf("asked the name of %q", ip)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		e, _ = DetectExternal()
		if e.FQDN != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("name never filled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e.Host() != "mycelium-fraffo.tail1234.ts.net" {
		t.Fatalf("host %q", e.Host())
	}
	for h, want := range map[string]bool{
		"100.101.102.103:8000":                 true,
		"mycelium-fraffo.tail1234.ts.net:8000": true,
		"MYCELIUM-FRAFFO.tail1234.ts.net.":     true,
		"192.168.1.10:8000":                    false,
		"":                                     false,
	} {
		if e.Matches(h) != want {
			t.Errorf("Matches(%q) = %v", h, !want)
		}
	}
	select {
	case ip := <-asked:
		t.Fatalf("name asked again for %q", ip)
	default:
	}
}
