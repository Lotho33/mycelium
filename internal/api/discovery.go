// LAN discovery for Pileus: a small UDP responder so a freshly-installed
// client can find mycelium's IP without the user typing it in. Replaces the
// old mDNS-via-Avahi approach (internal/managers/network.go), which never
// actually worked in the Docker deployment (no avahi-daemon in the image)
// and had a second, divergent implementation in the (since removed) bare-metal
// installer — two systems, neither reliable everywhere. UDP broadcast works the same way
// regardless of deployment (Docker port-publish forwards broadcast-received
// datagrams to the container like any other published UDP port) and needs
// nothing installed on the host.
package api

import (
	"encoding/json"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
	"mycelium/internal/pileus"
)

// discoveryPort is the UDP port mycelium listens on for discovery broadcasts.
// Keep in sync with the port mapping in docker-compose.yml /
// .devcontainer/docker-compose.dev.yml.
const discoveryPort = 51900

// discoveryMagic is the exact datagram a discovery request must contain.
// Anything else is silently dropped — keeps this socket from answering as a
// generic reflector for whatever else lands on the port.
const discoveryMagic = "MYCELIUM_DISCOVER_V1"

// discoveryInfo is the payload advertised to a client, whether it already
// knows the IP (GET /pileus/info) or is still looking for it (UDP
// broadcast) — same fields, one place to keep them in sync.
func discoveryInfo() map[string]any {
	fingerprint, tlsEnabled := pileus.TLSFingerprint()
	httpPort, err := strconv.Atoi(managers.Settings.GetString("server_port", "8000"))
	if err != nil {
		httpPort = 8000
	}
	return map[string]any{
		"name":                 "Mycelium",
		"version":              core.Version,
		"http_port":            httpPort,
		"grpc_port":            50051,
		"setup_done":           managers.Settings.IsSetupDone(),
		"grpc_tls":             tlsEnabled,
		"grpc_tls_fingerprint": fingerprint,
	}
}

// StartDiscovery launches the UDP discovery responder in the background.
// Best-effort: if the port can't be bound (already in use, no permission in
// this environment) it logs and the server carries on without LAN discovery
// — Pileus still works by typing the IP in manually, this is convenience,
// never a hard dependency.
func StartDiscovery() {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: discoveryPort, IP: net.IPv4zero})
	if err != nil {
		log.Printf("[discovery] UDP :%d non disponibile, discovery LAN disattivata: %v", discoveryPort, err)
		return
	}
	log.Printf("[discovery] in ascolto su UDP :%d", discoveryPort)
	go runDiscoveryResponder(conn)
}

// discoveryRateLimit caps replies to one per source IP per window, so a
// single sender on the LAN can't turn this into a UDP reflection/
// amplification tool by spamming the port — everything past the first hit
// in the window is dropped silently, not queued or errored.
var (
	discoveryRateMu sync.Mutex
	discoveryLastAt = map[string]time.Time{}
)

// 400ms (was 2s): a client that loses the first datagram or its reply on a
// noisy LAN retries a couple of times a few hundred ms apart — a 2s lockout
// swallowed those retries and the client fell back to a slow TCP probe of
// stale candidates. 400ms still caps a spoofed-source reflector to ~2.5
// replies/s/IP (~150 B each), which is not amplification-useful.
const discoveryRateWindow = 400 * time.Millisecond

func discoveryAllowed(ip string) bool {
	discoveryRateMu.Lock()
	defer discoveryRateMu.Unlock()
	now := time.Now()
	if last, ok := discoveryLastAt[ip]; ok && now.Sub(last) < discoveryRateWindow {
		return false
	}
	discoveryLastAt[ip] = now
	// Opportunistic cleanup so this map doesn't grow unbounded on a busy LAN
	// — checked only when it's already gotten big, not on every packet.
	if len(discoveryLastAt) > 256 {
		for k, t := range discoveryLastAt {
			if now.Sub(t) > discoveryRateWindow {
				delete(discoveryLastAt, k)
			}
		}
	}
	return true
}

func runDiscoveryResponder(conn *net.UDPConn) {
	buf := make([]byte, 512)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Socket closed (process shutting down) or a fatal read error —
			// either way there's nothing to retry into.
			return
		}
		func() {
			defer core.Guard("api/discovery-responder-packet")
			if string(buf[:n]) != discoveryMagic || !discoveryAllowed(src.IP.String()) {
				return
			}
			reply, err := json.Marshal(discoveryInfo())
			if err != nil {
				return
			}
			conn.WriteToUDP(reply, src) //nolint:errcheck
		}()
	}
}
