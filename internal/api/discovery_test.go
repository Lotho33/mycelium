package api

import (
	"encoding/json"
	"net"
	"testing"
	"time"
)

// A responder bound to an ad-hoc port answers a valid magic datagram with the
// discovery JSON and ignores anything else.
func TestDiscoveryResponder(t *testing.T) {
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("bind responder: %v", err)
	}
	defer srv.Close()
	go runDiscoveryResponder(srv)

	cli, err := net.DialUDP("udp4", nil, srv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	// valid magic → JSON reply with the fields Pileus needs
	if _, err := cli.Write([]byte(discoveryMagic)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	_ = cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := cli.Read(buf)
	if err != nil {
		t.Fatalf("no reply to valid magic: %v", err)
	}
	var info map[string]any
	if err := json.Unmarshal(buf[:n], &info); err != nil {
		t.Fatalf("reply not JSON (%q): %v", buf[:n], err)
	}
	if info["name"] != "Mycelium" {
		t.Errorf("name = %v; want Mycelium", info["name"])
	}
	for _, k := range []string{"grpc_port", "http_port", "version", "setup_done"} {
		if _, ok := info[k]; !ok {
			t.Errorf("reply missing %q: %v", k, info)
		}
	}

	// garbage → silently dropped, no reply
	if _, err := cli.Write([]byte("not the magic")); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := cli.Read(buf); err == nil {
		t.Errorf("responder replied to a non-magic datagram")
	}
}

// discoveryAllowed: one hit per source per window, then it reopens.
func TestDiscoveryRateLimit(t *testing.T) {
	const ip = "203.0.113.42" // TEST-NET-3, won't collide with a real probe
	if !discoveryAllowed(ip) {
		t.Fatal("first hit rejected")
	}
	if discoveryAllowed(ip) {
		t.Fatal("second hit within the window was allowed")
	}
	time.Sleep(discoveryRateWindow + 100*time.Millisecond)
	if !discoveryAllowed(ip) {
		t.Fatal("hit after the window was still rejected")
	}
}
