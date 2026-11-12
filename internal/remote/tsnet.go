//go:build !noremote

package remote

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Available is true: this build includes remote access (see stub.go).
const Available = true

// Manager owns at most one tsnet node at a time.
type Manager struct {
	mu     sync.Mutex
	cfg    Config
	status Status
	cancel context.CancelFunc
	done   chan struct{}
	lc     *local.Client
}

// New returns a stopped manager.
func New(cfg Config) *Manager {
	return &Manager{cfg: cfg, status: Status{State: StateDisabled, Hostname: cfg.Hostname, ControlURL: cfg.ControlURL}}
}

// Status returns the current snapshot; it never blocks on the network.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	s.CertDomains = slices.Clone(s.CertDomains)
	return s
}

// Running reports whether a node is started (in any state but disabled).
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancel != nil
}

// IsTailnetHost reports whether host (a Host header value or an address,
// with or without port) names this node on the tailnet: its FQDN or one of
// its Tailscale IPs. Used to tell tailnet clients from LAN ones.
func (m *Manager) IsTailnetHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return false
	}
	s := m.Status()
	if s.State != StateRunning {
		return false
	}
	return host == strings.ToLower(s.FQDN) || host == s.IPv4 || host == s.IPv6
}

// Start brings the node up in the background (replacing any running node).
// It returns at once: a slow or unreachable control server must never delay
// the caller.
func (m *Manager) Start(o Options) {
	m.Stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Hostname, m.cfg.ControlURL = o.Hostname, o.ControlURL
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	m.status = Status{State: StateStarting, Hostname: o.Hostname, ControlURL: o.ControlURL}
	go m.run(ctx, m.cfg, m.done)
}

// Stop shuts the node down (keeping its state on disk) and waits for it,
// bounded.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		log.Printf("[remote] arresto del nodo oltre 15s, si prosegue")
	}
	m.mu.Lock()
	if m.cancel == nil {
		m.status = Status{State: StateDisabled, Hostname: m.cfg.Hostname, ControlURL: m.cfg.ControlURL}
	}
	m.mu.Unlock()
}

// Logout logs the node out of the tailnet (best effort: the control server
// may be unreachable), stops it and wipes its state, so the next start asks
// for a new login and creates a new machine.
func (m *Manager) Logout(ctx context.Context) error {
	m.mu.Lock()
	lc := m.lc
	m.mu.Unlock()
	var logoutErr error
	if lc != nil {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		logoutErr = lc.Logout(lctx)
		cancel()
	}
	m.Stop()
	if err := wipeDir(m.cfg.Dir); err != nil {
		return err
	}
	if logoutErr != nil {
		log.Printf("[remote] logout dal control server non riuscito (stato locale cancellato comunque): %v", logoutErr)
	}
	return nil
}

// Relogin asks the control server for a fresh login link (the previous one
// expired, or the user wants to log in with another account).
func (m *Manager) Relogin(ctx context.Context) error {
	m.mu.Lock()
	lc := m.lc
	m.mu.Unlock()
	if lc == nil {
		return errors.New("accesso remoto non attivo")
	}
	return lc.StartLoginInteractive(ctx)
}

func wipeDir(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("cancellazione stato tailscale: %w", err)
	}
	return nil
}

// noLogUpload stops tsnet from uploading its own logs to Tailscale's log
// service. Process-wide, set once before the first node starts.
var noLogUpload sync.Once

// run owns one tsnet.Server from Start to Close (Close must never run
// concurrently with Start, so both happen here).
func (m *Manager) run(ctx context.Context, cfg Config, done chan struct{}) {
	defer close(done)
	noLogUpload.Do(envknob.SetNoLogsNoSupport)
	if !m.waitHostTailscale(ctx) {
		return
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		m.fail(ctx, fmt.Errorf("cartella di stato: %w", err))
		return
	}
	srv := &tsnet.Server{
		Dir:        cfg.Dir,
		Hostname:   cfg.Hostname,
		ControlURL: cfg.ControlURL,
		AuthKey:    cfg.AuthKey,
		Ephemeral:  false, // same machine, same name across restarts
		Logf:       func(string, ...any) {},
		UserLogf:   userLogf,
	}
	defer srv.Close()

	if err := srv.Start(); err != nil {
		m.fail(ctx, err)
		return
	}
	lc, err := srv.LocalClient()
	if err != nil {
		m.fail(ctx, err)
		return
	}
	m.mu.Lock()
	if ctx.Err() == nil {
		m.lc = lc
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.lc == lc {
			m.lc = nil
		}
		m.mu.Unlock()
	}()

	servers, err := m.serve(srv, lc, cfg)
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(sctx)
		}
	}()
	if err != nil {
		m.fail(ctx, err)
		return
	}

	go m.certLoop(ctx, lc)
	m.watch(ctx, lc)
}

// hostTailscale is HostTailscale (a var for tests); hostTailscaleRecheck is
// how often a waiting node looks again.
var (
	hostTailscale        = HostTailscale
	hostTailscaleRecheck = 15 * time.Second
)

// waitHostTailscale holds the node in StateHostTailscale while another
// Tailscale runs on this machine (see the state's doc); false when ctx ends
// first.
func (m *Manager) waitHostTailscale(ctx context.Context) bool {
	logged := false
	for {
		ifc := hostTailscale()
		if ifc == "" {
			if logged {
				log.Printf("[remote] il Tailscale separato non c'è più: avvio il nodo di mycelium")
			}
			return true
		}
		if !logged {
			log.Printf("[remote] Tailscale è già attivo su questa macchina (interfaccia %s): il nodo di mycelium parte quando viene spento", ifc)
			logged = true
		}
		m.mu.Lock()
		if ctx.Err() == nil {
			m.status.State = StateHostTailscale
			m.status.Err = ifc
		}
		m.mu.Unlock()
		if !sleepCtx(ctx, hostTailscaleRecheck) {
			return false
		}
	}
}

// userLogf keeps tsnet's user-facing messages, minus the login reminder it
// repeats every 5 s (the dashboard shows the link; the manager logs it once).
func userLogf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if strings.Contains(msg, "To start this tsnet server") || strings.HasPrefix(msg, "AuthLoop:") {
		return
	}
	log.Printf("[remote] %s", msg)
}

// serve opens the tailnet listeners. Never Funnel: tsnet only exposes a
// listener publicly through ListenFunnel, which this package doesn't call.
//
//	:443          the LAN handler over TLS (certificate managed by tsnet)
//	:<http port>  the LAN handler in clear (WireGuard already encrypts it):
//	              native clients mint /proxy URLs with the address they
//	              dialed, i.e. <tailscale ip>:<http port>
//	:80           redirect to https://<fqdn> once the certificate is ready
//	:<grpc port>  the same gRPC server, with the same pinned TLS cert
func (m *Manager) serve(srv *tsnet.Server, lc *local.Client, cfg Config) ([]*http.Server, error) {
	var servers []*http.Server
	listen := func(port string) (net.Listener, error) {
		ln, err := srv.Listen("tcp", ":"+port)
		if err != nil {
			return nil, fmt.Errorf("listener tailnet :%s: %w", port, err)
		}
		return ln, nil
	}
	used := map[string]bool{}
	start := func(port string, h http.Handler, tlsOn bool) error {
		if used[port] {
			log.Printf("[remote] porta %s già usata sulla tailnet, salto", port)
			return nil
		}
		used[port] = true
		ln, err := listen(port)
		if err != nil {
			return err
		}
		s := cfg.NewServer(h)
		servers = append(servers, s)
		if tlsOn {
			s.TLSConfig = &tls.Config{GetCertificate: lc.GetCertificate, MinVersion: tls.VersionTLS12}
			go func() {
				if err := s.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
					log.Printf("[remote] https: %v", err)
				}
			}()
		} else {
			go func() {
				if err := s.Serve(ln); err != nil && err != http.ErrServerClosed {
					log.Printf("[remote] http :%s: %v", port, err)
				}
			}()
		}
		return nil
	}
	if err := start("443", cfg.Handler, true); err != nil {
		return servers, err
	}
	if err := start(cfg.HTTPPort, cfg.Handler, false); err != nil {
		return servers, err
	}
	if err := start("80", m.redirectHandler(cfg.Handler), false); err != nil {
		return servers, err
	}
	if cfg.GRPC != nil && cfg.GRPCPort != "" && !used[cfg.GRPCPort] {
		ln, err := listen(cfg.GRPCPort)
		if err != nil {
			return servers, err
		}
		go func() {
			// Returns when the listener closes with the node; the gRPC
			// server keeps serving the LAN.
			if err := cfg.GRPC.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("[remote] grpc tailnet fermato: %v", err)
			}
		}()
	}
	return servers, nil
}

// redirectHandler sends http://<fqdn>/… to https://<fqdn>/… once the
// certificate is ready; before that (and for requests by IP) it serves the
// page as is.
func (m *Manager) redirectHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := m.Status()
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if s.HTTPSReady && s.FQDN != "" && strings.EqualFold(strings.TrimSuffix(host, "."), s.FQDN) {
			http.Redirect(w, r, "https://"+s.FQDN+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// watch follows the IPN bus until ctx ends, refreshing the snapshot on each
// state change and every minute (key expiry, certificate domains).
func (m *Manager) watch(ctx context.Context, lc *local.Client) {
	notify := make(chan *ipn.Notify, 8)
	go func() {
		defer close(notify)
		for ctx.Err() == nil {
			w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
			if err != nil {
				if !sleepCtx(ctx, 2*time.Second) {
					return
				}
				continue
			}
			for {
				n, err := w.Next()
				if err != nil {
					break
				}
				select {
				case notify <- &n:
				case <-ctx.Done():
				}
			}
			w.Close()
		}
	}()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	m.refresh(ctx, lc, "")
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-notify:
			if !ok {
				return
			}
			errMsg := ""
			if n.ErrMessage != nil {
				errMsg = *n.ErrMessage
			}
			m.refresh(ctx, lc, errMsg)
		case <-tick.C:
			m.refresh(ctx, lc, "")
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// refresh reads the backend status and maps it to a Status.
func (m *Manager) refresh(ctx context.Context, lc *local.Client, errMsg string) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	st, err := lc.StatusWithoutPeers(sctx)
	cancel()
	if ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lc != lc {
		return // a newer node replaced this one
	}
	prev := m.status
	if err != nil {
		m.status.Err = err.Error()
		return
	}
	next := snapshot(st, prev)
	if errMsg != "" {
		next.Err = errMsg
	}
	if next.State == StateNeedsLogin && next.AuthURL != "" && next.AuthURL != prev.AuthURL {
		log.Printf("[remote] accesso a Tailscale richiesto: apri la dashboard (Rete → Accesso da fuori casa) oppure %s", next.AuthURL)
	}
	if next.State == StateRunning && prev.State != StateRunning {
		log.Printf("[remote] collegato alla tailnet come %s (%s)", next.FQDN, next.IPv4)
	}
	m.status = next
}

// snapshot maps an ipnstate.Status onto Status, carrying over what only the
// manager knows (requested name, control URL, certificate readiness).
func snapshot(st *ipnstate.Status, prev Status) Status {
	s := Status{
		State:      stateFromBackend(st.BackendState),
		Hostname:   prev.Hostname,
		ControlURL: prev.ControlURL,
	}
	if s.State == StateNeedsLogin {
		s.AuthURL = st.AuthURL
	}
	if st.CurrentTailnet != nil {
		s.Tailnet = st.CurrentTailnet.Name
		s.MagicDNS = st.CurrentTailnet.MagicDNSEnabled
	}
	if self := st.Self; self != nil && s.State == StateRunning {
		s.FQDN = strings.TrimSuffix(self.DNSName, ".")
		for _, ip := range self.TailscaleIPs {
			if ip.Is4() && s.IPv4 == "" {
				s.IPv4 = ip.String()
			} else if ip.Is6() && s.IPv6 == "" {
				s.IPv6 = ip.String()
			}
		}
		if self.KeyExpiry != nil && !self.KeyExpiry.IsZero() {
			t := *self.KeyExpiry
			s.KeyExpiry = &t
		}
	}
	if s.State == StateRunning {
		s.CertDomains = slices.Clone(st.CertDomains)
		// Still ready only for the same name.
		if prev.FQDN == s.FQDN {
			s.HTTPSReady, s.CertErr = prev.HTTPSReady, prev.CertErr
		}
	}
	return s
}

// stateFromBackend maps the ipn backend state names.
func stateFromBackend(backend string) State {
	switch backend {
	case ipn.Running.String():
		return StateRunning
	case ipn.NeedsLogin.String(), ipn.Stopped.String():
		return StateNeedsLogin
	case ipn.NeedsMachineAuth.String():
		return StateNeedsApproval
	case ipn.InUseOtherUser.String():
		return StateError
	default: // NoState, Starting
		return StateStarting
	}
}

// certLoop pre-fetches the certificate for the node's FQDN as soon as the
// tailnet allows it, so the first visit from a phone doesn't wait on ACME;
// renewals are tsnet's job.
func (m *Manager) certLoop(ctx context.Context, lc *local.Client) {
	for ctx.Err() == nil {
		s := m.Status()
		wait := 15 * time.Second
		if s.State == StateRunning && s.FQDN != "" && !s.HTTPSReady && slices.Contains(s.CertDomains, s.FQDN) {
			cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			_, _, err := lc.CertPair(cctx, s.FQDN)
			cancel()
			if ctx.Err() != nil {
				return
			}
			m.mu.Lock()
			if m.lc == lc && m.status.FQDN == s.FQDN {
				if err != nil {
					m.status.CertErr = err.Error()
					wait = 2 * time.Minute
				} else {
					m.status.HTTPSReady, m.status.CertErr = true, ""
					log.Printf("[remote] certificato HTTPS pronto per %s", s.FQDN)
				}
			}
			m.mu.Unlock()
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// fail records a start error, unless the node was stopped meanwhile.
func (m *Manager) fail(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	log.Printf("[remote] errore: %v", err)
	m.mu.Lock()
	m.status.State = StateError
	m.status.Err = err.Error()
	m.mu.Unlock()
}
