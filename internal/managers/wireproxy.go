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

// WireGuard egress in userspace. The operator uploads plain WireGuard .conf
// files; mycelium stores each, forces its own [Socks5] section onto it and
// runs one `wireproxy` child process per config, each with a SOCKS5
// listener on its own loopback port. wireproxy needs no NET_ADMIN or kernel
// module. The binary ships in the image at wireproxyBinPath; elsewhere the
// feature needs it at that path.

const (
	// Loopback port window wireproxy processes bind their SOCKS5 listener on.
	wireproxyPortBase = 1081
	wireproxyPortMax  = 1099
)

var (
	// wireproxyBinPath is where the image installs wireproxy (a var for tests).
	wireproxyBinPath = "/usr/local/bin/wireproxy"
	// wireproxyStopTimeout is how long a SIGTERM'd process gets before SIGKILL
	// (a var for tests).
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
// section (bound to 127.0.0.1:port) onto it and writes it atomically.
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

// ─── subprocess registry ────────────────────────────────────────────────────
//
// One wireproxy child process per egress profile name. No auto-restart: a
// process that dies stays down until enabled/re-uploaded from the dashboard
// or the next boot (ReapplyWireproxyAtBoot).

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

// wireproxyIsRunning reports whether a subprocess is tracked as running.
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

// wireproxyStopProcess sends SIGTERM to the tracked process and waits up to
// wireproxyStopTimeout, then SIGKILL. No-op when nothing runs.
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

// wireproxyLogWriter prefixes every line with the profile name.
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

// wireproxyStartProcess (re)launches the subprocess for name from its saved
// .conf, stopping any previous instance (wireproxy has no reload).
func wireproxyStartProcess(name string) error {
	wireproxyStopProcess(name)

	confPath := wireproxyConfPath(name)
	if _, err := os.Stat(confPath); err != nil {
		return fmt.Errorf("config wireproxy per %q non trovata: %w", name, err)
	}

	cmd := exec.Command(wireproxyBinPath, "-c", confPath)
	setSysProcAttr(cmd) // Linux: dies with mycelium even without a graceful shutdown
	cmd.Stdout = &wireproxyLogWriter{name: name, out: os.Stdout}
	cmd.Stderr = &wireproxyLogWriter{name: name, out: os.Stderr}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("avvio wireproxy per %q: %w", name, err)
	}

	p := &wireproxyProc{cmd: cmd, running: true, done: make(chan struct{})}
	wireproxyRegMu.Lock()
	wireproxyReg[name] = p
	wireproxyRegMu.Unlock()

	// Reap the process and mark it stopped as soon as it exits.
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

// wireproxyDefaultMTU is forced onto [Interface] when the upload sets none:
// on links with a smaller path MTU the handshake works but full-size
// packets are dropped. 1280 (the IPv6 minimum) is safe everywhere.
const wireproxyDefaultMTU = 1280

// normalizeWireguardConf keeps [Interface]/[Peer] verbatim, drops any proxy
// sections of the upload so ours is authoritative, forces a conservative
// MTU when none is set, validates the essentials, and appends our [Socks5]
// bound to bindAddr.
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
