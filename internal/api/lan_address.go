// Home addresses of this server, for the dashboard: the private IPv4s of
// the LAN interfaces, and the "<name>.local" name when an mDNS responder
// (avahi, on the mini PCs) really answers for it. Shown next to the
// tailnet address, so the admin knows both ways in.
package api

import (
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"mycelium/internal/core"
)

// skippedIfacePrefixes are virtual interfaces that are not the home LAN:
// containers, bridges, VPNs and Tailscale.
var skippedIfacePrefixes = []string{"lo", "docker", "br-", "veth", "virbr", "cni", "flannel", "tailscale", "tun", "tap", "wg", "zt", "utun"}

var dockerNet = mustCIDR("172.16.0.0/12")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// lanIPv4s lists the private IPv4 addresses of the LAN interfaces, sorted.
// In a container on Docker's bridge network the only address is the
// container's own (172.16/12), useless from the LAN: left out there.
func lanIPv4s() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	inDocker := os.Getenv("MYCELIUM_DOCKER") == "1"
	var out []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || skippedIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || !ip.IsPrivate() || (inDocker && dockerNet.Contains(ip)) {
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}

func skippedIface(name string) bool {
	name = strings.ToLower(name)
	for _, p := range skippedIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ─── mDNS (.local) ───────────────────────────────────────────────────────────

const (
	mdnsTTL     = 5 * time.Minute
	mdnsTimeout = 800 * time.Millisecond
)

var mdnsCache struct {
	sync.Mutex
	name     string
	at       time.Time
	fetching bool
}

// mdnsName is "<name>.local" when an mDNS responder on the LAN answers for
// this machine, else "". Never blocks: it returns the cached answer and
// refreshes it in the background when older than mdnsTTL (the first call
// returns "" and the next ones the result).
func mdnsName() string {
	mdnsCache.Lock()
	defer mdnsCache.Unlock()
	if time.Since(mdnsCache.at) >= mdnsTTL && !mdnsCache.fetching {
		mdnsCache.fetching = true
		core.SafeGo("api/mdns-probe", func() {
			name := probeMDNS()
			mdnsCache.Lock()
			mdnsCache.name, mdnsCache.at, mdnsCache.fetching = name, time.Now(), false
			mdnsCache.Unlock()
		})
	}
	return mdnsCache.name
}

// mdnsCandidates are the names to try: the machine's hostname, then
// "mycelium" (the name the mini PC bundles give avahi).
func mdnsCandidates() []string {
	var out []string
	if h, err := os.Hostname(); err == nil {
		h = strings.ToLower(strings.TrimSuffix(strings.SplitN(h, ".", 2)[0], "."))
		if h != "" && h != "localhost" {
			out = append(out, h)
		}
	}
	if len(out) == 0 || out[0] != "mycelium" {
		out = append(out, "mycelium")
	}
	return out
}

// probeMDNS asks the LAN (224.0.0.251:5353) for each candidate's A record
// and keeps the first one answered with an address of this machine, so a
// different box called "mycelium" never counts.
func probeMDNS() string {
	own := map[string]bool{}
	for _, ip := range lanIPv4s() {
		own[ip] = true
	}
	if len(own) == 0 {
		return ""
	}
	for _, host := range mdnsCandidates() {
		if ip := mdnsLookupA(host + ".local."); ip != "" && own[ip] {
			return host + ".local"
		}
	}
	return ""
}

// mdnsLookupA sends one "legacy unicast" mDNS query (RFC 6762 §6.7: from an
// ephemeral port, so responders such as avahi answer straight back) and
// waits mdnsTimeout for an A record of fqdn. "" on no answer.
func mdnsLookupA(fqdn string) string {
	name, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return ""
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	msg, err := b.Finish()
	if err != nil {
		return ""
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return ""
	}
	defer conn.Close()
	if _, err := conn.WriteToUDP(msg, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}); err != nil {
		return ""
	}
	_ = conn.SetReadDeadline(time.Now().Add(mdnsTimeout))
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "" // timeout: nobody answers for that name
		}
		var p dnsmessage.Parser
		if _, err := p.Start(buf[:n]); err != nil {
			continue
		}
		_ = p.SkipAllQuestions()
		for {
			h, err := p.AnswerHeader()
			if err != nil {
				break
			}
			if h.Type == dnsmessage.TypeA && strings.EqualFold(h.Name.String(), fqdn) {
				if r, err := p.AResource(); err == nil {
					return net.IP(r.A[:]).String()
				}
			}
			if err := p.SkipAnswer(); err != nil {
				break
			}
		}
	}
}
