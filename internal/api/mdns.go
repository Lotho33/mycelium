// Minimal built-in mDNS (RFC 6762) responder: lets any device on the LAN —
// not just the Pileus app — resolve a name like "mycelium.local" from a
// plain browser address bar, with nothing installed on the mycelium host
// besides mycelium itself.
//
// This is NOT the same thing as discovery.go's UDP broadcast responder, and
// doesn't replace it: that one is a Pileus-internal protocol (a fixed magic
// datagram, JSON reply) the native app's own resolveGrpcHost() speaks —
// invisible to a browser, an OS's own mDNS stack, or a friend typing a URL.
// This one speaks the real mDNS wire protocol so *anything* mDNS-aware
// (Chrome, Android's own resolver, macOS/iOS Bonjour, `avahi-resolve` on a
// friend's Linux box) can look the name up, which is what actually lets
// someone type "https://mycelium.local" to reach the PWA install/trust
// pages without a real domain.
//
// mycelium tried real mDNS once before via a system avahi-daemon
// (internal/managers/network.go, since removed) and gave up on it — not
// because mDNS itself is a bad fit, but because depending on an *external*
// daemon that may not exist in a given deployment (the Docker image ships
// none) made it unreliable everywhere. This implementation has no such
// dependency: it's a small hand-rolled responder in the same spirit as
// discovery.go, using only golang.org/x/net (already a direct dependency)
// for multicast sockets and DNS message (de)serialisation.
//
// Deliberately minimal — RFC 6762 in full (probing, conflict detection,
// multicast replies so every listener can cache them, service browsing via
// PTR/_services._dns-sd) is a lot of protocol for a "type a name instead of
// an IP" convenience feature. This only answers A/ANY queries for the one
// configured hostname, unicast straight back to whoever asked — which is
// exactly what every mainstream mDNS *client* implementation actually does
// when it sends a query and gets an answer, even though it deviates from
// the RFC's "always reply via multicast" recommendation.
//
// Network requirement (impossible to satisfy from inside the process, so
// only documented, not worked around): mDNS depends on real IP multicast
// reaching this process's socket. That works with network_mode: host (or
// bare metal) — Docker's default bridge networking with published ports
// does NOT forward multicast traffic into a container, only ordinary
// unicast UDP/TCP on the ports you publish (this is exactly why
// discovery.go uses broadcast+a fixed port instead of mDNS in the first
// place). If mdns_hostname never resolves for anyone, this is the first
// thing to check.
package api

import (
	"log"
	"net"
	"strings"

	"golang.org/x/net/dns/dnsmessage"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

const mdnsPort = 5353

var mdnsGroupV4 = net.IPv4(224, 0, 0, 251)

// StartMDNS launches the responder in the background, best-effort — same
// posture as StartDiscovery: if it can't bind (already in use, no
// CAP_NET_RAW-adjacent permission in this environment, disabled by the
// operator) it logs and the server carries on. mdns_hostname/mdns_ip are
// plain settings (no dedicated admin API), read once here — like
// server_https, applying a change needs a restart, since the socket is
// opened at boot.
func StartMDNS() {
	if !core.ParseBoolish(managers.Settings.GetString("mdns_enabled", "true")) {
		log.Printf("[mdns] disattivato da impostazioni")
		return
	}
	hostname := strings.ToLower(strings.TrimSpace(managers.Settings.GetString("mdns_hostname", "mycelium")))
	hostname = strings.TrimSuffix(hostname, ".local")
	hostname = strings.TrimSuffix(hostname, ".")
	if hostname == "" {
		hostname = "mycelium"
	}
	fqdn := hostname + ".local."

	conn, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: mdnsGroupV4, Port: mdnsPort})
	if err != nil {
		log.Printf("[mdns] UDP multicast :%d non disponibile, risoluzione %q disattivata: %v", mdnsPort, fqdn, err)
		return
	}
	log.Printf("[mdns] in ascolto su %s:%d per %s", mdnsGroupV4, mdnsPort, fqdn)
	go runMDNSResponder(conn, fqdn)
}

func runMDNSResponder(conn *net.UDPConn, fqdn string) {
	buf := make([]byte, 9000) // mDNS allows jumbo-ish packets on modern LANs; well above any query we care about
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			// Socket closed (shutdown) or a fatal read error — nothing to retry into.
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		go func() {
			defer core.Guard("api/mdns-responder-packet")
			handleMDNSPacket(conn, src, pkt, fqdn)
		}()
	}
}

func handleMDNSPacket(conn *net.UDPConn, src *net.UDPAddr, pkt []byte, fqdn string) {
	var p dnsmessage.Parser
	hdr, err := p.Start(pkt)
	// A response, not a query — ignore (we're not a client of anyone else's
	// mDNS traffic, and answering a reply would just be noise/a reflection
	// risk).
	if err != nil || hdr.Response {
		return
	}
	questions, err := p.AllQuestions()
	if err != nil {
		return
	}
	if !anyQuestionMatches(questions, fqdn) {
		return
	}
	ip, ok := primaryLANIPv4()
	if !ok {
		return
	}
	reply, err := buildMDNSReply(hdr.ID, fqdn, ip)
	if err != nil {
		return
	}
	conn.WriteToUDP(reply, src) //nolint:errcheck
}

// anyQuestionMatches reports whether questions asks for fqdn (case-
// insensitive, as DNS names are) by A or ANY record — the only two query
// shapes this minimal responder answers.
func anyQuestionMatches(questions []dnsmessage.Question, fqdn string) bool {
	for _, q := range questions {
		if strings.EqualFold(q.Name.String(), fqdn) && (q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeALL) {
			return true
		}
	}
	return false
}

func buildMDNSReply(id uint16, fqdn string, ip net.IP) ([]byte, error) {
	name, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:            id,
		Response:      true,
		Authoritative: true,
	})
	b.EnableCompression()
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	var a [4]byte
	copy(a[:], ip.To4())
	err = b.AResource(
		dnsmessage.ResourceHeader{
			Name:  name,
			Class: dnsmessage.ClassINET,
			// Short TTL: this is a live "here's my current IP" answer, not a
			// stable record — a DHCP lease change should stop being cached
			// quickly rather than sending clients to a stale address for a
			// long time.
			TTL: 120,
		},
		dnsmessage.AResource{A: a},
	)
	if err != nil {
		return nil, err
	}
	return b.Finish()
}

// primaryLANIPv4 picks the server's own LAN-facing IPv4 address — an
// operator override (mdns_ip setting) always wins, since auto-detection is
// inherently a guess on a host with more than one interface (a Tailscale
// tailscale0, a Docker bridge alongside the real LAN NIC under
// network_mode: host, …). Auto-detection prefers a private (RFC 1918)
// address on an interface that isn't loopback/down and doesn't look like a
// container bridge — best-effort, same spirit as discoveryInfo() elsewhere:
// picking the wrong address just means the name doesn't resolve usefully,
// not a wrong/insecure answer.
func primaryLANIPv4() (net.IP, bool) {
	if override := strings.TrimSpace(managers.Settings.GetString("mdns_ip", "")); override != "" {
		if ip := net.ParseIP(override).To4(); ip != nil {
			return ip, true
		}
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "lo") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || !isPrivateIPv4(ip4) {
				continue
			}
			return ip4, true
		}
	}
	return nil, false
}

func isPrivateIPv4(ip net.IP) bool {
	return ip[0] == 10 ||
		(ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) ||
		(ip[0] == 192 && ip[1] == 168)
}
