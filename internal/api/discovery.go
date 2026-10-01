// LAN discovery for Pileus: a UDP responder so a client can find mycelium's
// address without the user typing it in. UDP broadcast works the same with
// or without Docker (a published UDP port receives broadcasts) and needs
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

// discoveryPort is the UDP port of discovery broadcasts (keep in sync with
// the compose files).
const discoveryPort = 51900

// discoveryMagic is the exact datagram a request must contain; anything else
// is dropped.
const discoveryMagic = "MYCELIUM_DISCOVER_V1"

// discoveryInfo is what a client is told, via GET /pileus/info or the UDP
// broadcast.
func discoveryInfo() map[string]any {
	fingerprint, tlsEnabled := pileus.TLSFingerprint()
	httpPort, err := strconv.Atoi(managers.Settings.GetString("server_port", "8000"))
	if err != nil {
		httpPort = 8000
	}
	// The port the gRPC server really listens on (same setting as main.go).
	grpcPort, err := strconv.Atoi(managers.Settings.GetString("pileus_grpc_port", "50051"))
	if err != nil {
		grpcPort = 50051
	}
	return map[string]any{
		"name":                 "Mycelium",
		"version":              core.Version,
		"http_port":            httpPort,
		"grpc_port":            grpcPort,
		"setup_done":           managers.Settings.IsSetupDone(),
		"grpc_tls":             tlsEnabled,
		"grpc_tls_fingerprint": fingerprint,
	}
}

// StartDiscovery launches the UDP responder in the background. Best-effort:
// if the port can't be bound it logs and carries on (the address can still
// be typed in).
func StartDiscovery() {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: discoveryPort, IP: net.IPv4zero})
	if err != nil {
		log.Printf("[discovery] UDP :%d non disponibile, discovery LAN disattivata: %v", discoveryPort, err)
		return
	}
	log.Printf("[discovery] in ascolto su UDP :%d", discoveryPort)
	go runDiscoveryResponder(conn)
}

// discoveryRateLimit allows one reply per source IP per window, so the port
// can't be used as a reflector.
var (
	discoveryRateMu sync.Mutex
	discoveryLastAt = map[string]time.Time{}
)

// Short enough for a client's quick retries on a lossy LAN, still capping a
// spoofed-source reflector at a few small replies per second.
const discoveryRateWindow = 400 * time.Millisecond

func discoveryAllowed(ip string) bool {
	discoveryRateMu.Lock()
	defer discoveryRateMu.Unlock()
	now := time.Now()
	if last, ok := discoveryLastAt[ip]; ok && now.Sub(last) < discoveryRateWindow {
		return false
	}
	discoveryLastAt[ip] = now
	// Cleanup only once the map has grown.
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
			// Socket closed or fatal read error.
			return
		}
		func() {
			defer core.Guard("api/discovery-responder-packet")
			// LAN only: off-LAN the reply (larger than the query) could only serve as
			// a reflector towards a spoofed source.
			if string(buf[:n]) != discoveryMagic || !lanUDPSource(src) || !discoveryAllowed(src.IP.String()) {
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

// lanUDPSource reports whether a UDP query from src may be answered: LAN
// addresses only (private, link-local, loopback, CGNAT), like the setup page.
func lanUDPSource(src *net.UDPAddr) bool {
	if src == nil || src.IP == nil {
		return false
	}
	ip := src.IP
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback() || setupCGNAT.Contains(ip)
}
