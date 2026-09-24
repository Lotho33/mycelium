package managers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"mycelium/internal/core"
)

// WireGuard-over-userspace egress. The operator uploads plain WireGuard .conf
// files (Mullvad, Proton, a self-hosted peer, …). mycelium stores each, forces
// a [Socks5] section onto it, and runs ONE `wireproxy` subprocess per config —
// launched directly by mycelium itself (no container, no Docker socket), each
// binding its own SOCKS5 proxy on a dedicated loopback port.
//
// wireproxy is pure userspace WireGuard: no NET_ADMIN, no kernel module, just
// a process opening outbound UDP and a local SOCKS5 listener — running it as a
// child process of mycelium instead of a separate container loses no
// meaningful isolation while dropping the /var/run/docker.sock mount entirely
// (write access to that socket is root-equivalent on the host).
//
// The binary ships inside the mycelium image at wireproxyBinPath (see
// Dockerfile); Docker is the only supported deployment. Outside it (e.g.
// `go run` in development) this feature needs wireproxy at that same path —
// everything else in mycelium runs fine without it.

const (
	// Loopback port window wireproxy processes bind their SOCKS5 listener on.
	wireproxyPortBase = 1081
	wireproxyPortMax  = 1099
)

var (
	// wireproxyBinPath is a var (not a const) so tests can point it at a fake
	// executable. Matches where the Dockerfile installs the real binary.
	wireproxyBinPath = "/usr/local/bin/wireproxy"
	// wireproxyStopTimeout is how long wireproxyStopProcess waits for a
	// SIGTERM'd process to exit before escalating to SIGKILL. A var (not a
	// const) so a test can shrink it instead of taking 5s to exercise the
	// kill-fallback path.
	wireproxyStopTimeout = 5 * time.Second
)

func wireproxyDir() string { return core.AppPath("data", "wireproxy") }

func wireproxyConfPath(name string) string {
	return filepath.Join(wireproxyDir(), sanitizeEgressName(name)+".conf")
}

// sanitizeEgressName keeps a profile name safe as a filename / registry-key
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
// section (bound to 127.0.0.1:port — the port this profile was assigned) onto
// it and writes it atomically.
func SaveWireproxyConf(name, raw string, port int) error {
	cfg, err := normalizeWireguardConf(raw, fmt.Sprintf("127.0.0.1:%d", port))
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

// assignWireproxyPort returns the loopback port for `name`: its current one if
// the profile already has it, else the lowest free port in the window not
// used by another wireproxy profile.
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

// ─── in-process subprocess registry ─────────────────────────────────────────
//
// Replaces the old Docker-sidecar lifecycle (CreateContainer/StartContainer/
// StopContainer/RestartContainer/ContainerRunning) with a plain map of
// wireproxy child processes, one per egress profile name. No auto-restart on
// crash (matching the old sidecars' RestartPolicy: "no"): a process that dies
// unexpectedly stays down until an explicit action (enable/re-upload from the
// dashboard, or the next boot's ReapplyWireproxyAtBoot) starts it again.

type wireproxyProc struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	running bool
	done    chan struct{} // closed once cmd.Wait() returns
}

var (
	wireproxyRegMu sync.Mutex
	wireproxyReg   = map[string]*wireproxyProc{}
)

// wireproxyIsRunning reports whether a subprocess is currently tracked as
// running for this profile — an in-memory check, no external call.
func wireproxyIsRunning(name string) bool {
	wireproxyRegMu.Lock()
	p := wireproxyReg[name]
	wireproxyRegMu.Unlock()
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// wireproxyStopProcess sends SIGTERM to the tracked process for `name` and
// waits up to wireproxyStopTimeout for it to exit, escalating to SIGKILL
// otherwise. No-op if nothing is tracked, or it already exited on its own
// (e.g. a crash) — this is not an error, just "already stopped".
func wireproxyStopProcess(name string) {
	wireproxyRegMu.Lock()
	p := wireproxyReg[name]
	wireproxyRegMu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	running := p.running
	proc := p.cmd.Process
	p.mu.Unlock()
	if !running || proc == nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(wireproxyStopTimeout):
		_ = proc.Kill()
		<-p.done
	}
}

// wireproxyLogWriter prefixes every line written to it with the profile name
// before forwarding to the underlying file — so interleaved output from
// several concurrent wireproxy processes stays attributable in the log.
type wireproxyLogWriter struct {
	name string
	out  *os.File
}

func (w *wireproxyLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintf(w.out, "[wireproxy:%s] %s\n", w.name, line)
	}
	return len(p), nil
}

// wireproxyStartProcess (re)launches the wireproxy subprocess for `name`,
// reading its already-saved .conf from disk. Any previous instance is stopped
// first — wireproxy has no reload signal, so picking up a changed config
// needs a fresh process, same as the old sidecar's stop+start restart.
func wireproxyStartProcess(name string) error {
	wireproxyStopProcess(name)

	confPath := wireproxyConfPath(name)
	if _, err := os.Stat(confPath); err != nil {
		return fmt.Errorf("config wireproxy per %q non trovata: %w", name, err)
	}

	cmd := exec.Command(wireproxyBinPath, "-c", confPath)
	setSysProcAttr(cmd) // Pdeathsig: SIGTERM on Linux — dies with mycelium even without a graceful shutdown
	cmd.Stdout = &wireproxyLogWriter{name: name, out: os.Stdout}
	cmd.Stderr = &wireproxyLogWriter{name: name, out: os.Stderr}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("avvio wireproxy per %q: %w", name, err)
	}

	p := &wireproxyProc{cmd: cmd, running: true, done: make(chan struct{})}
	wireproxyRegMu.Lock()
	wireproxyReg[name] = p
	wireproxyRegMu.Unlock()

	// Reap the process and flip its state to "not running" the moment it
	// exits — by itself (crash) or via wireproxyStopProcess's signal — so
	// WireproxyRunning reflects reality without polling anything. No
	// auto-restart here on purpose (see package doc comment above).
	core.SafeGo("managers/wireproxy-wait-"+name, func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
		close(p.done)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[wireproxy:%s] processo terminato: %v\n", name, err)
		} else {
			fmt.Fprintf(os.Stderr, "[wireproxy:%s] processo terminato\n", name)
		}
	})
	return nil
}

// ApplyWireproxyEgress brings the subprocess for one wireproxy profile into
// the wanted state: (re)started when enabled and configured, stopped
// otherwise.
func ApplyWireproxyEgress(p EgressProfile) error {
	if !p.Enabled || !WireproxyConfigured(p.Name) {
		wireproxyStopProcess(p.Name)
		return nil
	}
	if p.Port < wireproxyPortBase || p.Port > wireproxyPortMax {
		return fmt.Errorf("uscita %q senza porta assegnata — ri-carica il file .conf", p.Name)
	}
	return wireproxyStartProcess(p.Name)
}

// WireproxyRunning reports whether the subprocess for this egress is up.
func WireproxyRunning(name string) bool {
	return wireproxyIsRunning(name)
}

// DeleteWireproxyEgress stops the subprocess (if any) and removes the stored
// config.
func DeleteWireproxyEgress(name string) error {
	wireproxyStopProcess(name)
	wireproxyRegMu.Lock()
	delete(wireproxyReg, name)
	wireproxyRegMu.Unlock()
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
// the essentials, and appends our [Socks5] bound to bindAddr (the loopback
// address:port this profile was assigned — see assignWireproxyPort).
func normalizeWireguardConf(raw string, bindAddr string) (string, error) {
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
	return body + "\n\n[Socks5]\nBindAddress = " + bindAddr + "\n", nil
}
