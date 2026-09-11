package managers

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mycelium/internal/core"
)

// WireGuard-over-userspace egress. The operator uploads plain WireGuard .conf
// files (Mullvad, Proton, a self-hosted peer, …). mycelium stores each, forces
// a [Socks5] section onto it, and runs ONE `wireproxy-<name>` sidecar per
// config — created on demand over the Docker socket (Fase C: N configs, N
// sidecars), each publishing its SOCKS5 proxy on a dedicated host port.
//
// wireproxy is pure userspace WireGuard: no NET_ADMIN, no kernel module. The
// configs live on the `mycelium_wireproxy` named volume (declared in
// docker-compose.prod.yml with an explicit name:), mounted read-only into every
// sidecar — so mycelium never needs to know its own host-side path.

const (
	// wireproxyBinInImage is where the mycelium image ships the wireproxy
	// binary (see Dockerfile) — the sidecars run mycelium's own image with
	// this as entrypoint.
	wireproxyBinInImage = "/usr/local/bin/wireproxy"
	wireproxyVolume     = "mycelium_wireproxy"
	// wireproxyBindInside is the address wireproxy binds INSIDE its container;
	// each sidecar maps 127.0.0.1:<hostPort> → this.
	wireproxyBindInside = "0.0.0.0:1080"
	// Host-port window for the sidecars' published SOCKS5 proxies.
	wireproxyPortBase = 1081
	wireproxyPortMax  = 1099
)

// wireproxyImage is the image the egress sidecars run. Default: mycelium's own
// image (it bundles the wireproxy binary — see Dockerfile — and is already on
// the box, no third-party registry to reach). Overridable via
// MYCELIUM_WIREPROXY_IMAGE for a dedicated wireproxy image if ever preferred.
func wireproxyImage() string {
	if v := strings.TrimSpace(os.Getenv("MYCELIUM_WIREPROXY_IMAGE")); v != "" {
		return v
	}
	if self := SelfImageRef(); self != "" {
		return self
	}
	return "ghcr.io/windtf/wireproxy:latest" // last resort
}

// wireproxyEntrypoint is the command the sidecar runs. When the image is
// mycelium's own, we override the entrypoint to the bundled wireproxy binary;
// a dedicated wireproxy image already has the right entrypoint so we pass none.
func wireproxyEntrypoint() []string {
	if strings.TrimSpace(os.Getenv("MYCELIUM_WIREPROXY_IMAGE")) != "" {
		return nil // trust the dedicated image's own ENTRYPOINT
	}
	return []string{wireproxyBinInImage}
}

func wireproxyDir() string { return core.AppPath("data", "wireproxy") }

func wireproxyConfPath(name string) string {
	return filepath.Join(wireproxyDir(), sanitizeEgressName(name)+".conf")
}

func wireproxyContainerFor(name string) string {
	return "wireproxy-" + sanitizeEgressName(name)
}

// sanitizeEgressName keeps a profile name safe as a filename / container-name
// fragment.
func sanitizeEgressName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// WireproxyConfigured reports whether a config file exists for this egress.
func WireproxyConfigured(name string) bool {
	fi, err := os.Stat(wireproxyConfPath(name))
	return err == nil && fi.Size() > 0
}

// SaveWireproxyConf validates a raw WireGuard config, forces our [Socks5]
// section onto it and writes it atomically.
func SaveWireproxyConf(name, raw string) error {
	cfg, err := normalizeWireguardConf(raw)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(wireproxyDir(), 0o755); err != nil {
		return err
	}
	dst := wireproxyConfPath(name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(cfg), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// assignWireproxyPort returns the host port for `name`: its current one if the
// profile already has it, else the lowest free port in the window not used by
// another wireproxy profile.
func assignWireproxyPort(list []EgressProfile, name string) (int, error) {
	used := map[int]bool{}
	for _, p := range list {
		if p.Kind == "wireproxy" {
			if p.Name == name && p.Port >= wireproxyPortBase && p.Port <= wireproxyPortMax {
				return p.Port, nil
			}
			if p.Port != 0 {
				used[p.Port] = true
			}
		}
	}
	for port := wireproxyPortBase; port <= wireproxyPortMax; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("nessuna porta libera per l'uscita WireGuard (max %d)", wireproxyPortMax-wireproxyPortBase+1)
}

// ApplyWireproxyEgress brings the sidecar for one wireproxy profile into the
// wanted state: created+started (or restarted, to pick up a config change) when
// enabled and configured, stopped otherwise.
func ApplyWireproxyEgress(p EgressProfile) error {
	cn := wireproxyContainerFor(p.Name)
	if !p.Enabled || !WireproxyConfigured(p.Name) {
		_ = StopContainer(cn)
		return nil
	}
	if p.Port < wireproxyPortBase || p.Port > wireproxyPortMax {
		return fmt.Errorf("uscita %q senza porta assegnata — ri-carica il file .conf", p.Name)
	}
	confInside := "/etc/wireproxy/" + sanitizeEgressName(p.Name) + ".conf"
	if ContainerExists(cn) {
		return RestartContainer(cn) // stop+start → re-reads the (possibly changed) conf
	}
	if err := CreateContainer(ContainerSpec{
		Name:          cn,
		Image:         wireproxyImage(),
		Entrypoint:    wireproxyEntrypoint(),
		Cmd:           []string{"-c", confInside},
		Labels:        map[string]string{"xyz.mycelium.managed": "wireproxy", "xyz.mycelium.egress": p.Name},
		Binds:         []string{wireproxyVolume + ":/etc/wireproxy:ro"},
		Ports:         map[string]string{"1080/tcp": fmt.Sprintf("127.0.0.1:%d", p.Port)},
		RestartPolicy: "unless-stopped",
	}); err != nil {
		return err
	}
	return StartContainer(cn)
}

// WireproxyRunning reports whether the sidecar for this egress is up.
func WireproxyRunning(name string) bool {
	return ContainerRunning(wireproxyContainerFor(name))
}

// DeleteWireproxyEgress removes the sidecar and its stored config.
func DeleteWireproxyEgress(name string) error {
	if err := RemoveContainer(wireproxyContainerFor(name)); err != nil {
		return err
	}
	if err := os.Remove(wireproxyConfPath(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// wireproxyDefaultMTU is forced onto [Interface] when the upload doesn't set
// one. Provider configs default to 1420; on a link with a smaller path MTU
// (Starlink, PPPoE, another tunnel in front) the WireGuard handshake — small
// packets — completes, but the first full-size TLS record inside the tunnel
// gets black-holed → "context deadline exceeded" on every request. 1280 is the
// IPv6 minimum, safe everywhere, and what the Mullvad/Proton mobile clients
// ship. Costs a little throughput, buys reliability.
const wireproxyDefaultMTU = 1280

// normalizeWireguardConf keeps [Interface]/[Peer] (and any comments) verbatim,
// drops any proxy sections the upload might carry ([Socks5]/[http]/tunnels) so
// ours is authoritative, forces a conservative MTU if none is set, validates
// the essentials, and appends our [Socks5].
func normalizeWireguardConf(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	dropped := map[string]bool{
		"socks5": true, "http": true,
		"tcpclienttunnel": true, "tcpservertunnel": true,
	}

	var kept []string
	section := ""
	ifaceEnd := -1 // index in `kept` right after the [Interface] block
	var hasIface, hasPriv, hasPeer, hasPub, hasEndpoint, hasMTU bool

	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			if section == "interface" && ifaceEnd == -1 {
				ifaceEnd = len(kept) // first header after [Interface]
			}
			section = strings.ToLower(strings.TrimSpace(t[1 : len(t)-1]))
			if section == "interface" {
				hasIface = true
			}
			if section == "peer" {
				hasPeer = true
			}
			if !dropped[section] {
				kept = append(kept, line)
			}
			continue
		}
		if dropped[section] {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(strings.SplitN(t, "=", 2)[0]))
		switch {
		case section == "interface" && key == "privatekey":
			hasPriv = true
		case section == "interface" && key == "mtu":
			hasMTU = true
		case section == "peer" && key == "publickey":
			hasPub = true
		case section == "peer" && key == "endpoint":
			hasEndpoint = true
		}
		kept = append(kept, line)
	}
	if section == "interface" && ifaceEnd == -1 {
		ifaceEnd = len(kept)
	}

	switch {
	case !hasIface || !hasPriv:
		return "", fmt.Errorf("config non valida: manca [Interface] con PrivateKey")
	case !hasPeer || !hasPub || !hasEndpoint:
		return "", fmt.Errorf("config non valida: manca [Peer] con PublicKey ed Endpoint")
	}

	if !hasMTU && ifaceEnd >= 0 {
		mtuLine := fmt.Sprintf("MTU = %d", wireproxyDefaultMTU)
		kept = append(kept[:ifaceEnd], append([]string{mtuLine}, kept[ifaceEnd:]...)...)
	}

	body := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	return body + "\n\n[Socks5]\nBindAddress = " + wireproxyBindInside + "\n", nil
}
