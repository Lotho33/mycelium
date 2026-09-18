package managers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// NetworkManager gestisce l'accesso remoto privato via Tailscale. Il server è
// pensato per restare sulla LAN o su una rete mesh Tailscale: non espone
// alcun endpoint pubblico su Internet. La scoperta LAN (trovare l'IP del
// server senza doverlo digitare) è gestita altrove — vedi
// internal/api/discovery.go (risponditore UDP broadcast) — non più qui: era
// mDNS via Avahi, rimosso perché nella pratica non ha mai funzionato nel
// deployment Docker (nessun avahi-daemon nell'immagine, vedi Dockerfile) e
// nel deployment bare-metal (install.sh) esisteva una seconda implementazione
// duplicata e mai rinominata dal codename originale del progetto — due
// sistemi paralleli per lo stesso scopo, funzionanti solo per metà dei casi.
var Network = &NetworkManager{}

type NetworkManager struct{}

// ─────────────────────────────────────────────────────────────────────────────
// Tailscale — accesso remoto privato (mesh), senza esposizione pubblica
// ─────────────────────────────────────────────────────────────────────────────

type TailscaleStatus struct {
	IP        string `json:"ip"`
	Hostname  string `json:"hostname"`
	Connected bool   `json:"connected"`
}

func (n *NetworkManager) getTailscaleIP() (string, error) {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

var tailscaleSampler = newLiveSampler()

// GetTailscaleStats samples CPU/RAM of the tailscaled daemon. This only works
// when tailscaled is visible in the same PID namespace as this process — true
// on a bare-metal deployment (install.sh, tailscaled as a sibling system
// service) but NOT when mycelium runs in its own Docker container, since
// tailscaled isn't installed/managed there (see the package doc comment
// above: the CLI is shelled out to, no daemon bundled). ok is false when
// tailscaled can't be found, which is an expected state, not an error.
// findProcessPID scans /proc for a process whose cmdline contains any of
// markers, skipping entries for which skip(cmdline) returns true (skip may be
// nil). Returns 0 on non-Linux or if not found. Works for any process visible
// in this process's PID namespace, not just children — e.g. tailscaled on a
// bare-metal deployment where it runs as a sibling system service.
func findProcessPID(markers []string, skip func(cmdline string) bool) int {
	if runtime.GOOS != "linux" {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		cmdline := string(bytes.ReplaceAll(data, []byte{0}, []byte(" ")))
		for _, name := range markers {
			if !strings.Contains(cmdline, name) {
				continue
			}
			if skip != nil && skip(cmdline) {
				continue
			}
			return pid
		}
	}
	return 0
}

func (n *NetworkManager) GetTailscaleStats() (ResourceStats, bool) {
	pid := findProcessPID([]string{"tailscaled"}, nil)
	if pid == 0 {
		return ResourceStats{}, false
	}
	return tailscaleSampler.sample(pid)
}

func (n *NetworkManager) TailscaleStatus() TailscaleStatus {
	ip, err := n.getTailscaleIP()
	if err != nil || ip == "" {
		return TailscaleStatus{}
	}
	out, _ := exec.Command("tailscale", "status", "--json").Output()
	var data struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	json.Unmarshal(out, &data)
	hostname := strings.TrimSuffix(data.Self.DNSName, ".")
	return TailscaleStatus{IP: ip, Hostname: hostname, Connected: true}
}

// TailscaleUp avvia Tailscale e restituisce l'URL di autenticazione se necessario.
func (n *NetworkManager) TailscaleUp(authKey string) (authURL string, err error) {
	args := []string{"up", "--accept-dns=false"}
	if authKey != "" {
		args = append(args, "--auth-key="+authKey)
	}
	cmd := exec.Command("tailscale", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Cerca URL di autenticazione nell'output
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "https://login.tailscale.com") {
				return strings.TrimSpace(line), nil
			}
		}
		return "", fmt.Errorf("tailscale up: %s", string(out))
	}
	return "", nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Setup rete — orchestrazione (mDNS + Tailscale)
// ─────────────────────────────────────────────────────────────────────────────

type NetworkSetupConfig struct {
	TailscaleAuthKey string `json:"tailscale_auth_key,omitempty"`
}

type NetworkSetupProgress struct {
	Step    string `json:"step"`
	Message string `json:"message"`
	Done    bool   `json:"done"`
	Error   string `json:"error,omitempty"`
}

// RunNetworkSetup configura la scoperta LAN e l'accesso remoto privato via
// Tailscale, inviando i progressi sul channel. Non configura alcun endpoint
// pubblico: l'accesso da fuori LAN passa esclusivamente dalla mesh Tailscale.
func (n *NetworkManager) RunNetworkSetup(cfg NetworkSetupConfig, progress chan<- NetworkSetupProgress) {
	defer close(progress)

	send := func(step, msg string, done bool, err error) {
		e := ""
		if err != nil {
			e = err.Error()
		}
		progress <- NetworkSetupProgress{Step: step, Message: msg, Done: done, Error: e}
	}

	// Tailscale
	send("tailscale", "Avvio Tailscale...", false, nil)
	authURL, err := n.TailscaleUp(cfg.TailscaleAuthKey)
	if err != nil {
		send("tailscale", "", true, err)
		return
	}
	if authURL != "" {
		send("tailscale_auth", authURL, false, nil)
		// Aspetta che Tailscale sia connesso (max 5 min)
		for i := 0; i < 60; i++ {
			time.Sleep(5 * time.Second)
			if s := n.TailscaleStatus(); s.Connected {
				break
			}
			if i == 59 {
				send("tailscale", "", true, fmt.Errorf("timeout attesa autenticazione Tailscale"))
				return
			}
		}
	}
	ts := n.TailscaleStatus()
	send("tailscale", fmt.Sprintf("Tailscale connesso: %s", ts.IP), true, nil)

	// 3. Salva stato
	Settings.Save(map[string]any{
		"tailscale_ip": ts.IP,
	})

	doneMsg := "Setup completato!"
	if ts.IP != "" {
		doneMsg = fmt.Sprintf("Setup completato! Accesso remoto via Tailscale: http://%s:%s", ts.IP, Settings.GetString("server_port", "8000"))
	}
	send("done", doneMsg, true, nil)
}

// WriteSSE scrive un evento SSE sul ResponseWriter.
func WriteSSE(w http.ResponseWriter, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// NetworkStatus restituisce lo stato corrente della rete.
func (n *NetworkManager) NetworkStatus() map[string]any {
	ts := n.TailscaleStatus()
	return map[string]any{
		"tailscale": ts,
	}
}
