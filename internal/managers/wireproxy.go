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
	// wireproxyApplyMu serializes start/stop/delete: two concurrent applies
	// of the same profile would otherwise both start a process on its port.
	wireproxyApplyMu sync.Mutex
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
	wireproxyApplyMu.Lock()
	defer wireproxyApplyMu.Unlock()
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
	wireproxyApplyMu.Lock()
	defer wireproxyApplyMu.Unlock()
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

// wireproxyDefaultDNS is used, through the tunnel, when the upload sets none.
const wireproxyDefaultDNS = "1.1.1.1"

// wgAllowedKeys is everything normalizeWireguardConf carries over, per
// section (lower-case key → canonical spelling). Anything else in the upload
// is dropped: wireproxy also reads proxy/tunnel sections ([Socks5], [http],
// [SNI], [*Tunnel]…) that would open listeners of their own outside our
// control.
var wgAllowedKeys = map[string]map[string]string{
	"interface": {"privatekey": "PrivateKey", "address": "Address", "dns": "DNS", "mtu": "MTU"},
	"peer": {"publickey": "PublicKey", "presharedkey": "PresharedKey", "endpoint": "Endpoint",
		"allowedips": "AllowedIPs", "persistentkeepalive": "PersistentKeepalive"},
}

// normalizeWireguardConf rebuilds the upload from its [Interface] and [Peer]
// keys only (see wgAllowedKeys), forces a conservative MTU and a DNS server
// when none is set (without one wireproxy resolves names outside the
// tunnel), validates the essentials, and appends our [Socks5] bound to
// bindAddr.
func normalizeWireguardConf(raw string, bindAddr string) (string, error) {
	type section struct {
		name  string
		lines []string
		keys  map[string]bool
	}
	var secs []*section
	var cur *section
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		// Comments run to the end of the line, as in go-ini (wireproxy's parser).
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "[") {
			if !strings.HasSuffix(t, "]") {
				return "", fmt.Errorf("config non valida: intestazione di sezione malformata %q", t)
			}
			name := strings.ToLower(strings.TrimSpace(t[1 : len(t)-1]))
			cur = nil
			if wgAllowedKeys[name] != nil {
				cur = &section{name: name, keys: map[string]bool{}}
				secs = append(secs, cur)
			}
			continue
		}
		if cur == nil {
			continue // outside a kept section
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return "", fmt.Errorf("config non valida: riga senza '=' in [%s]", cur.name)
		}
		canon, allowed := wgAllowedKeys[cur.name][strings.ToLower(strings.TrimSpace(k))]
		if !allowed {
			continue
		}
		cur.keys[strings.ToLower(canon)] = true
		cur.lines = append(cur.lines, canon+" = "+strings.TrimSpace(v))
	}

	var iface *section
	peers := 0
	for _, sec := range secs {
		switch sec.name {
		case "interface":
			if iface != nil {
				return "", fmt.Errorf("config non valida: più di una sezione [Interface]")
			}
			iface = sec
		case "peer":
			if !sec.keys["publickey"] || !sec.keys["endpoint"] {
				return "", fmt.Errorf("config non valida: manca [Peer] con PublicKey ed Endpoint")
			}
			peers++
		}
	}
	switch {
	case iface == nil || !iface.keys["privatekey"]:
		return "", fmt.Errorf("config non valida: manca [Interface] con PrivateKey")
	case peers == 0:
		return "", fmt.Errorf("config non valida: manca [Peer] con PublicKey ed Endpoint")
	}
	if !iface.keys["mtu"] {
		iface.lines = append(iface.lines, fmt.Sprintf("MTU = %d", wireproxyDefaultMTU))
	}
	if !iface.keys["dns"] {
		iface.lines = append(iface.lines, "DNS = "+wireproxyDefaultDNS)
	}

	var b strings.Builder
	for _, sec := range secs {
		b.WriteString("[" + map[string]string{"interface": "Interface", "peer": "Peer"}[sec.name] + "]\n")
		b.WriteString(strings.Join(sec.lines, "\n"))
		b.WriteString("\n\n")
	}
	b.WriteString("[Socks5]\nBindAddress = " + bindAddr + "\n")
	return b.String(), nil
}
