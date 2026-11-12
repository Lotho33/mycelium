package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"mycelium/internal/core"
	"mycelium/internal/managers"
	"mycelium/internal/pileus"
	"mycelium/internal/remote"

	"rsc.io/qr"
)

// Remote access through the operator's tailnet (internal/remote,
// docs/remote-access.md): dashboard → Rete → "Accesso da fuori casa".

const (
	remoteEnabledKey    = "remote_access_enabled"
	remoteHostnameKey   = "remote_hostname"
	remoteControlURLKey = "remote_control_url"
	remoteDefaultHost   = "mycelium"
)

// remoteAccess is what the handlers need of *remote.Manager (a fake in tests).
type remoteAccess interface {
	Status() remote.Status
	Running() bool
	Start(o remote.Options)
	Stop()
	Logout(ctx context.Context) error
	Relogin(ctx context.Context) error
	IsTailnetHost(host string) bool
}

var (
	remoteMu  sync.Mutex // serializes enable/disable/logout/settings
	remoteAcc remoteAccess
)

// InitRemoteAccess builds the manager around the LAN handler and gRPC server
// and, when the feature is on, starts the node in the background. Called
// once by main after the LAN listeners are up. A Tailscale already running
// on the machine (remote.External) is used with or without the feature, and
// in builds without it.
func InitRemoteAccess(cfg remote.Config) {
	if !remote.Available {
		setRemoteAccess(nil)
		return
	}
	cfg.Dir = core.AppPath("data", "tailscale")
	cfg.AuthKey = firstEnv("MYCELIUM_REMOTE_AUTHKEY", "TS_AUTHKEY")
	cfg.Hostname, cfg.ControlURL = remoteHostname(), remoteControlURL()
	m := remote.New(cfg)
	setRemoteAccess(m)
	if managers.Settings.GetBool(remoteEnabledKey, false) {
		log.Printf("[remote] accesso da fuori casa attivo: collegamento alla tailnet in background")
		m.Start(remoteOptions())
	}
	if e, ok := detectExternal(); ok {
		if e.Connected() {
			log.Printf("[remote] Tailscale già presente sulla macchina (%s, %s): usato per l'accesso da fuori casa", e.Interface, e.IPv4)
		} else {
			log.Printf("[remote] Tailscale presente sulla macchina (%s) ma non collegato: va collegato lì (tailscale up)", e.Interface)
		}
	}
}

// detectExternal is remote.DetectExternal (a var for tests).
var detectExternal = remote.DetectExternal

// tailnetAddress is where this server answers on a tailnet right now: its
// own node when running, else the Tailscale already on the machine.
func tailnetAddress(m remoteAccess) (host, fqdn string, ok bool) {
	if m != nil {
		if s := m.Status(); s.State == remote.StateRunning && s.IPv4 != "" {
			return s.IPv4, s.FQDN, true
		}
	}
	if e, ok := detectExternal(); ok && e.IPv4 != "" {
		return e.IPv4, e.FQDN, true
	}
	return "", "", false
}

func setRemoteAccess(m remoteAccess) {
	remoteMu.Lock()
	remoteAcc = m
	remoteMu.Unlock()
	pileus.SetTailnetHostMatcher(func(h string) bool {
		if m != nil && m.IsTailnetHost(h) {
			return true
		}
		e, ok := detectExternal()
		return ok && e.Matches(h)
	})
	pileus.SetRemoteInfo(func() pileus.RemoteInfo {
		host, fqdn, ok := tailnetAddress(m)
		if !ok {
			return pileus.RemoteInfo{}
		}
		return pileus.RemoteInfo{Host: host, FQDN: fqdn}
	})
}

// StopRemoteAccess stops the node at shutdown (its state stays on disk).
func StopRemoteAccess() {
	remoteMu.Lock()
	m := remoteAcc
	remoteMu.Unlock()
	if m != nil {
		m.Stop()
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func remoteHostname() string {
	if v := firstEnv("MYCELIUM_REMOTE_HOSTNAME"); v != "" {
		return strings.ToLower(v)
	}
	if v := strings.TrimSpace(managers.Settings.GetString(remoteHostnameKey, "")); v != "" {
		return v
	}
	return remoteDefaultHost
}

func remoteControlURL() string {
	if v := firstEnv("MYCELIUM_REMOTE_CONTROL_URL"); v != "" {
		return v
	}
	return strings.TrimSpace(managers.Settings.GetString(remoteControlURLKey, ""))
}

// remoteOptions is how the node starts, from the settings (and env).
func remoteOptions() remote.Options {
	return remote.Options{
		Hostname:   remoteHostname(),
		ControlURL: remoteControlURL(),
	}
}

// validControlURL accepts "" (Tailscale) or an absolute http(s) URL.
func validControlURL(v string) bool {
	if v == "" {
		return true
	}
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

// RegisterRemoteRoutes mounts /admin/remote/*.
func RegisterRemoteRoutes(mux *http.ServeMux) {
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/remote/status", auth(getRemoteStatus))
	mux.HandleFunc("POST /admin/remote/enable", auth(enableRemote))
	mux.HandleFunc("POST /admin/remote/disable", auth(disableRemote))
	mux.HandleFunc("POST /admin/remote/logout", rateLimitMiddleware(wipeLimiter, auth(logoutRemote)))
	mux.HandleFunc("POST /admin/remote/relogin", auth(reloginRemote))
	mux.HandleFunc("PUT /admin/remote/settings", auth(saveRemoteSettings))
	mux.HandleFunc("GET /admin/remote/qr.svg", auth(remoteQR))
}

func currentRemote() remoteAccess {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	return remoteAcc
}

type remoteStatusResponse struct {
	remote.Status
	Enabled bool `json:"enabled"`
	// AppURL is the address to open on the phone, set only once HTTPS works:
	// nobody should install an http:// PWA.
	AppURL         string `json:"app_url,omitempty"`
	WebAppReady    bool   `json:"webapp_installed"`
	ConsoleURL     string `json:"console_url,omitempty"`
	DNSConsoleURL  string `json:"dns_console_url,omitempty"`
	HostnameEnv    bool   `json:"hostname_env"`
	ControlURLEnv  bool   `json:"control_url_env"`
	AuthKeyFromEnv bool   `json:"authkey_env"`
	Available      bool   `json:"available"`
	// External: the Tailscale already on the machine, in use (state
	// "external").
	External *remote.External `json:"external,omitempty"`
}

func remoteStatusSnapshot() remoteStatusResponse {
	resp := remoteStatusResponse{
		Enabled:        managers.Settings.GetBool(remoteEnabledKey, false),
		HostnameEnv:    firstEnv("MYCELIUM_REMOTE_HOSTNAME") != "",
		ControlURLEnv:  firstEnv("MYCELIUM_REMOTE_CONTROL_URL") != "",
		AuthKeyFromEnv: firstEnv("MYCELIUM_REMOTE_AUTHKEY", "TS_AUTHKEY") != "",
	}
	if _, err := os.Stat(core.AppPath("data", "pileus-web", "index.html")); err == nil {
		resp.WebAppReady = true
	}
	m := currentRemote()
	if m == nil {
		resp.Status = remote.Status{State: remote.StateDisabled, Hostname: remoteHostname(), ControlURL: remoteControlURL()}
	} else {
		resp.Available = true
		resp.Status = m.Status()
		if resp.State == remote.StateDisabled {
			resp.Hostname, resp.ControlURL = remoteHostname(), remoteControlURL()
		}
		if resp.HTTPSReady && resp.FQDN != "" {
			resp.AppURL = "https://" + resp.FQDN + "/app/"
		}
	}
	// No node of mycelium's own running, but a Tailscale on the machine: use
	// that one. http, on the LAN port: it holds no certificate for mycelium.
	if resp.State == remote.StateDisabled || resp.State == remote.StateHostTailscale {
		if e, ok := detectExternal(); ok {
			ext := e
			resp.External = &ext
			resp.State = remote.StateExternal
			if !e.Connected() {
				resp.State = remote.StateExternalOffline
			}
			resp.FQDN, resp.IPv4, resp.IPv6 = e.FQDN, e.IPv4, e.IPv6
			resp.Err = ""
			if h := e.Host(); h != "" {
				resp.AppURL = "http://" + net.JoinHostPort(h, managers.Settings.GetString("server_port", "8000")) + "/app/"
			}
		}
	}
	if resp.ControlURL == "" {
		resp.ConsoleURL = "https://login.tailscale.com/admin/machines"
		resp.DNSConsoleURL = "https://login.tailscale.com/admin/dns"
	}
	return resp
}

func getRemoteStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, remoteStatusSnapshot())
}

func remoteUnavailable(w http.ResponseWriter) bool {
	if currentRemote() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "accesso remoto non disponibile su questo server"})
		return true
	}
	return false
}

func enableRemote(w http.ResponseWriter, r *http.Request) {
	if remoteUnavailable(w) {
		return
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	if err := managers.Settings.SaveInternal(map[string]any{remoteEnabledKey: "true"}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	// Start also restarts a node left in the error state ("Riprova").
	remoteAcc.Start(remoteOptions())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func disableRemote(w http.ResponseWriter, r *http.Request) {
	if remoteUnavailable(w) {
		return
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	if err := managers.Settings.SaveInternal(map[string]any{remoteEnabledKey: "false"}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	remoteAcc.Stop()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// logoutRemote unlinks the server from the tailnet account and forgets the
// node; if the feature stays on, a fresh node asks for a new login.
func logoutRemote(w http.ResponseWriter, r *http.Request) {
	if remoteUnavailable(w) {
		return
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	if err := remoteAcc.Logout(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if managers.Settings.GetBool(remoteEnabledKey, false) {
		remoteAcc.Start(remoteOptions())
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func reloginRemote(w http.ResponseWriter, r *http.Request) {
	if remoteUnavailable(w) {
		return
	}
	if err := currentRemote().Relogin(r.Context()); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func saveRemoteSettings(w http.ResponseWriter, r *http.Request) {
	if remoteUnavailable(w) {
		return
	}
	var body struct {
		Hostname   *string `json:"hostname"`
		ControlURL *string `json:"control_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "richiesta non valida"})
		return
	}
	save := map[string]any{}
	if body.Hostname != nil {
		h := strings.ToLower(strings.TrimSpace(*body.Hostname))
		if h == "" {
			h = remoteDefaultHost
		}
		if !remote.ValidHostname(h) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "nome non valido: solo lettere minuscole, cifre e trattini (max 63)"})
			return
		}
		save[remoteHostnameKey] = h
	}
	if body.ControlURL != nil {
		c := strings.TrimRight(strings.TrimSpace(*body.ControlURL), "/")
		if !validControlURL(c) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": "indirizzo del control server non valido (https://…, vuoto per Tailscale)"})
			return
		}
		save[remoteControlURLKey] = c
	}
	if len(save) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	if err := managers.Settings.SaveInternal(save); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	// Applied by restarting the node, not the process.
	if remoteAcc.Running() {
		remoteAcc.Start(remoteOptions())
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// remoteQR renders the app URL (for=app) or the login link (for=login) as
// an SVG QR code. Only those two: it is not a general-purpose encoder.
func remoteQR(w http.ResponseWriter, r *http.Request) {
	s := remoteStatusSnapshot()
	var text string
	switch r.URL.Query().Get("for") {
	case "login":
		text = s.AuthURL
	default:
		text = s.AppURL
	}
	if text == "" {
		http.Error(w, "niente da mostrare", http.StatusNotFound)
		return
	}
	svg, err := qrSVG(text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(svg)
}

// qrSVG draws text as a QR code: one path, 4-module quiet zone.
func qrSVG(text string) ([]byte, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, err
	}
	const quiet = 4
	n := c.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return []byte(b.String()), nil
}
