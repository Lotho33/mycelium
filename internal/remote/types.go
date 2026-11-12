// Package remote is the optional "access from outside home" feature: the
// server joins the operator's own Tailscale tailnet (or a Headscale one)
// in-process through tsnet, and serves the same HTTP handler and gRPC server
// it serves on the LAN on the tailnet too, with a stable name
// (<host>.<tailnet>.ts.net) and a certificate browsers trust.
//
// Design: docs/remote-access.md. Nothing else in the code imports
// tailscale.com; the rest of the server reads Manager.Status().
//
// The node never uses Funnel: nothing here is reachable from the public
// Internet, only from devices of the tailnet.
package remote

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// State is the user-facing state of the node.
type State string

const (
	StateDisabled      State = "disabled"
	StateStarting      State = "starting"
	StateNeedsLogin    State = "needs_login"
	StateNeedsApproval State = "needs_approval"
	StateRunning       State = "running"
	StateError         State = "error"
	// StateHostTailscale: another Tailscale (a tailscaled container or the
	// host's own) is up on this machine. The node waits for it to stop
	// rather than run beside it: in the appliance stacks that daemon keeps
	// its state in the same data/tailscale directory, so both would be the
	// same node. Once it is gone the node starts, taking over that state
	// (same name and address, no new login) when it is there.
	StateHostTailscale State = "host_tailscale"
	// StateExternal: the Tailscale already on the machine is used (see
	// External); reported by the API while the node isn't running.
	StateExternal State = "external"
	// StateExternalOffline: that Tailscale is installed but not logged in
	// (External.Connected is false). It is still the one to use: the
	// dashboard explains how to log it in on the machine.
	StateExternalOffline State = "external_offline"
)

// Status is a snapshot of the node, cheap to take and safe to hand to the
// dashboard: it never holds the auth key.
type Status struct {
	State State `json:"state"`
	// Hostname is the requested machine name; FQDN is the one the control
	// server actually assigned (e.g. "mycelium-1.tail1234.ts.net" when
	// "mycelium" was taken). Only FQDN is ever used to build URLs.
	Hostname   string `json:"hostname"`
	ControlURL string `json:"control_url,omitempty"`
	AuthURL    string `json:"auth_url,omitempty"`
	FQDN       string `json:"fqdn,omitempty"`
	IPv4       string `json:"ipv4,omitempty"`
	IPv6       string `json:"ipv6,omitempty"`
	Tailnet    string `json:"tailnet,omitempty"`
	MagicDNS   bool   `json:"magic_dns"`
	// CertDomains is empty when HTTPS certificates aren't enabled in the
	// tailnet; HTTPSReady flips once the certificate for FQDN is issued.
	CertDomains []string   `json:"cert_domains,omitempty"`
	HTTPSReady  bool       `json:"https_ready"`
	CertErr     string     `json:"cert_error,omitempty"`
	KeyExpiry   *time.Time `json:"key_expiry,omitempty"`
	Err         string     `json:"error,omitempty"`
}

// Options choose how a node starts.
type Options struct {
	Hostname   string
	ControlURL string // "" = Tailscale's own control server
}

// GRPCServer is the part of *grpc.Server the manager needs.
type GRPCServer interface {
	Serve(net.Listener) error
}

// Config is what the node serves and where it keeps its state.
type Config struct {
	// Dir holds the node state (private key included). Never exported in
	// a backup: two machines restoring it would claim the same node.
	Dir        string
	Hostname   string
	ControlURL string // "" = Tailscale's own control server
	AuthKey    string // optional pre-auth key (headless login), env only

	// Handler is the same handler the LAN listener serves; NewServer builds
	// an *http.Server around it with the LAN server's timeouts and limits.
	Handler   http.Handler
	NewServer func(http.Handler) *http.Server
	GRPC      GRPCServer
	HTTPPort  string // the LAN HTTP port, also served on the tailnet
	GRPCPort  string
}

// ValidHostname reports whether h is a usable machine name: 1–63 chars of
// lowercase letters, digits and inner hyphens.
func ValidHostname(h string) bool {
	if len(h) == 0 || len(h) > 63 || h[0] == '-' || h[len(h)-1] == '-' {
		return false
	}
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// tailscaleULA is Tailscale's IPv6 range (fd7a:115c:a1e0::/48); tailscaleCGNAT
// its IPv4 one (100.64.0.0/10).
var (
	tailscaleULA   = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
	tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")
)

// External is a Tailscale node running on this machine outside mycelium: a
// tailscaled container of the compose, or the host's own. With host
// networking mycelium already answers on its addresses, so it can serve
// "away from home" through it, with no node of its own. It can't get an
// HTTPS certificate through it (that takes the daemon's local API, which
// isn't shared with mycelium): the app is served over http, inside the
// tailnet's WireGuard encryption.
type External struct {
	Interface string `json:"interface"`
	IPv4      string `json:"ipv4,omitempty"`
	IPv6      string `json:"ipv6,omitempty"`
	// FQDN is the node's MagicDNS name, read in the background from the
	// tailnet resolver; "" until known, or when MagicDNS is off.
	FQDN string `json:"fqdn,omitempty"`
}

// ExternalTTL is how long a detection is reused: callers include the
// per-request proxy URL check.
const externalTTL = 15 * time.Second

var ext struct {
	sync.Mutex
	at      time.Time
	val     External
	ok      bool
	ptrIP   string // address the FQDN below belongs to
	ptrName string
	ptrBusy bool
}

// ifaceInfo is the part of a network interface the detection reads.
type ifaceInfo struct {
	Name  string
	Up    bool
	Addrs []string // CIDR strings
}

func systemInterfaces() ([]ifaceInfo, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]ifaceInfo, 0, len(ifs))
	for _, ifc := range ifs {
		info := ifaceInfo{Name: ifc.Name, Up: ifc.Flags&net.FlagUp != 0}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			info.Addrs = append(info.Addrs, a.String())
		}
		out = append(out, info)
	}
	return out, nil
}

// interfacesFn and lookupPTR are swapped in tests.
var (
	interfacesFn = systemInterfaces
	lookupPTR    = magicDNSName
)

// DetectExternal reports the Tailscale node running on this machine outside
// mycelium, if any. Matched by interface name (tailscale0, the default) or
// by a Tailscale IPv6 address (a renamed TUN), never by 100.64/10 alone,
// which ISPs use for CGNAT too. tsnet itself creates no interface, so
// mycelium's own node is never mistaken for one. Cheap: cached for 15 s.
func DetectExternal() (External, bool) {
	ext.Lock()
	defer ext.Unlock()
	if time.Since(ext.at) < externalTTL {
		return ext.val, ext.ok
	}
	ext.at = time.Now()
	ext.val, ext.ok = scanInterfaces()
	if ext.ok && ext.val.IPv4 != "" {
		if ext.ptrIP == ext.val.IPv4 {
			ext.val.FQDN = ext.ptrName
		} else if !ext.ptrBusy {
			ext.ptrBusy = true
			go resolveExternalName(lookupPTR, ext.val.IPv4)
		}
	}
	return ext.val, ext.ok
}

// resolveExternalName fills the MagicDNS name of ip (best effort; the next
// detection picks it up).
func resolveExternalName(lookup func(string) string, ip string) {
	name := lookup(ip)
	ext.Lock()
	defer ext.Unlock()
	ext.ptrBusy = false
	ext.ptrIP, ext.ptrName = ip, name
	ext.at = time.Time{} // next call re-reads with the name
}

// magicDNSName asks the tailnet's resolver (100.100.100.100, reachable
// through the node's interface) for ip's name: "host.tailXXXX.ts.net".
func magicDNSName(ip string) string {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, "udp4", "100.100.100.100:53")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	names, err := r.LookupAddr(ctx, ip)
	if err != nil {
		return ""
	}
	for _, n := range names {
		n = strings.TrimSuffix(strings.ToLower(n), ".")
		if strings.HasSuffix(n, ".ts.net") || strings.Count(n, ".") >= 2 {
			return n
		}
	}
	return ""
}

func scanInterfaces() (External, bool) {
	ifs, err := interfacesFn()
	if err != nil {
		return External{}, false
	}
	for _, ifc := range ifs {
		if !ifc.Up {
			continue
		}
		var v4, v6 string
		for _, a := range ifc.Addrs {
			pfx, err := netip.ParsePrefix(a)
			if err != nil {
				continue
			}
			ip := pfx.Addr()
			switch {
			case ip.Is4() && tailscaleCGNAT.Contains(ip) && v4 == "":
				v4 = ip.String()
			case ip.Is6() && tailscaleULA.Contains(ip) && v6 == "":
				v6 = ip.String()
			}
		}
		// A logged-out tailscaled keeps its tailscale0 up with no tailnet
		// address: still the machine's Tailscale (see External.Connected).
		if strings.HasPrefix(ifc.Name, "tailscale") || v6 != "" {
			return External{Interface: ifc.Name, IPv4: v4, IPv6: v6}, true
		}
	}
	return External{}, false
}

// HostTailscale names the interface of a Tailscale node running on this
// machine outside mycelium ("" when there is none).
func HostTailscale() string {
	e, ok := DetectExternal()
	if !ok {
		return ""
	}
	return e.Interface
}

// Matches reports whether host (a Host header value or an address, with or
// without port) names this external node.
func (e External) Matches(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	return host != "" && (host == e.IPv4 || host == e.IPv6 || (e.FQDN != "" && host == e.FQDN))
}

// Connected reports whether the node holds a tailnet address. A
// tailscaled that is installed but logged out keeps its interface without
// one: it is still the Tailscale to use (mycelium's own node doesn't start
// beside it), it just has to be logged in on the machine first.
func (e External) Connected() bool { return e.IPv4 != "" || e.IPv6 != "" }

// Host is the address to give clients: the MagicDNS name when known, else
// the IPv4.
func (e External) Host() string {
	if e.FQDN != "" {
		return e.FQDN
	}
	return e.IPv4
}
