// Interactive browser session (noVNC): lets an operator watch and drive a
// real, headed browser running inside the companion browser service — routed
// through the same egress as any VPN-routed plugin — to complete an upstream's
// interactive verification (e.g. a CAPTCHA) by hand once. The browser service
// persists the resulting cookies (its per-registrable-domain, per-egress
// cookie jar) and reuses them automatically on later automated calls, so this
// is a one-off operator action, not something a plugin calls at runtime.
//
// Every request the operator's browser makes — the noVNC static client, the
// VNC websocket itself — is proxied through mycelium; the browser never talks
// to the browser service directly (it isn't reachable from outside the
// container network anyway, but this also keeps one consistent admin-auth
// boundary instead of a second one to reason about).
package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// dockerAwareDialContext returns a DialContext that resolves hostnames via
// the container engine's own resolver (127.0.0.11 on Docker) instead of
// net.DefaultResolver — which cmd/server/main.go's init() permanently points
// at Cloudflare 1.1.1.1 for scraping reliability, and which can therefore
// never resolve a Docker-internal name like "cobweb". Same fix already
// applied to InitRedis/ProxyDialer/ConnectBrowserClient (see
// internal/managers/browser_client.go) — every NEW client reaching a
// Docker-internal hostname needs it too, easy to forget once. Returns nil
// (caller keeps the default dialer) outside Docker.
func dockerAwareDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	if os.Getenv("MYCELIUM_DOCKER") != "1" {
		return nil
	}
	d := &net.Dialer{Timeout: 10 * time.Second, Resolver: &net.Resolver{PreferGo: true}}
	return d.DialContext
}

// RegisterVPNSessionRoutes wires the manual-session endpoints onto mux.
func RegisterVPNSessionRoutes(mux *http.ServeMux) {
	auth := adminAuthMiddleware
	mux.HandleFunc("POST /admin/vpn/session/start", auth(startVPNSession))
	mux.HandleFunc("POST /admin/vpn/session/{id}/close", auth(closeVPNSession))
	mux.HandleFunc("POST /admin/vpn/session/{id}/discard", auth(discardVPNSession))
	mux.HandleFunc("GET /admin/vpn/session/{id}/vnc-ws", auth(vpnSessionVNCWS))
	mux.HandleFunc("GET /admin/vpn/session/current", auth(currentVPNSession))
	mux.HandleFunc("GET /admin/vpn/novnc/", auth(vpnNoVNCStatic))
	mux.HandleFunc("GET /admin/vpn/jar", auth(vpnJarList))
	mux.HandleFunc("DELETE /admin/vpn/jar/{domain}", auth(vpnJarDelete))
	mux.HandleFunc("GET /admin/vpn/challenges", auth(vpnChallengeList))
	mux.HandleFunc("DELETE /admin/vpn/challenges/{domain}", auth(vpnChallengeDismiss))
}

// GET /admin/vpn/challenges → domains that returned an interactive
// verification (hit during a sniff or an HLS-proxy fetch) and are waiting for
// the operator to complete it. The dashboard renders one "Risolvi" per entry.
func vpnChallengeList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": managers.PendingChallenges()})
}

// DELETE /admin/vpn/challenges/{domain} → dismiss it from the list (a solve
// also clears it automatically on the next successful sniff).
func vpnChallengeDismiss(w http.ResponseWriter, r *http.Request) {
	managers.ClearChallengeDomain(r.PathValue("domain"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /admin/vpn/jar → [{domain, egress, stale, expires_at, last_ok, …}]
// The dashboard's "Cookie & sfide risolte" card: every domain cobweb has a
// solved challenge/login for, with freshness so a stale one can be re-solved.
func vpnJarList(w http.ResponseWriter, r *http.Request) {
	c := requireCobweb(w)
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	entries, err := c.JarList(ctx)
	if err != nil {
		log.Printf("[vpn/jar] list: %v", err)
		http.Error(w, "jar non disponibile: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// DELETE /admin/vpn/jar/{domain}?egress=<key> → drops the stored state for
// one (domain, egress) pair. `egress` is the value from GET /admin/vpn/jar;
// without it cobweb only ever matches the "direct" entry.
func vpnJarDelete(w http.ResponseWriter, r *http.Request) {
	c := requireCobweb(w)
	if c == nil {
		return
	}
	domain := r.PathValue("domain")
	egress := r.URL.Query().Get("egress")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	removed, err := c.JarDelete(ctx, domain, egress)
	if err != nil {
		log.Printf("[vpn/jar] delete %s (egress=%q): %v", domain, egress, err)
		http.Error(w, "eliminazione fallita: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !removed {
		http.Error(w, "nessuna voce corrispondente da eliminare", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /admin/vpn/session/current → {id, target_url} if cobweb has a manual
// session open, or {id: ""} if not. Only one session may exist at a time,
// and its id otherwise lives only in the admin browser's own JS state — this
// lets the dashboard recover it after a refresh or a dropped vnc-ws instead
// of leaving the admin stuck with a 409 on every start attempt and no way to
// discover the id to close it with.
func currentVPNSession(w http.ResponseWriter, r *http.Request) {
	c := requireCobweb(w)
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	id, targetURL, err := c.CurrentSession(ctx)
	if err != nil {
		log.Printf("[vpn/session] current: %v", err)
		http.Error(w, "stato sessione non disponibile: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "target_url": targetURL})
}

func requireCobweb(w http.ResponseWriter) *managers.BrowserServiceClient {
	c := managers.BrowserClient
	if c == nil {
		http.Error(w, "cobweb non configurato", http.StatusServiceUnavailable)
		return nil
	}
	return c
}

// POST /admin/vpn/session/start {url, direct?} → {id, target_url}
// By default routes through the configured WARP proxy (engine.LuaPlugins.
// GetProxyAddr()) — the whole point of this tool is authorizing a domain
// for the SAME IP that automated VPN-routed plugin calls will later use; solving
// a challenge on the direct IP wouldn't help those at all.
// `direct: true` is the deliberate escape hatch for a diagnostic session with
// NO proxy at all (e.g. "does this play with literally nothing active" —
// there was previously no way to get that without a proxy address to point
// at, direct or otherwise): it skips proxyAddr entirely and opens the
// session on the box's own IP.
func startVPNSession(w http.ResponseWriter, r *http.Request) {
	c := requireCobweb(w)
	if c == nil {
		return
	}
	var body struct {
		URL    string `json:"url"`
		Direct bool   `json:"direct"`
	}
	if err := readJSONBody(r, &body); err != nil || body.URL == "" {
		http.Error(w, "url mancante", http.StatusBadRequest)
		return
	}
	proxyAddr := ""
	if !body.Direct {
		proxyAddr = engine.LuaPlugins.GetProxyAddr()
		if proxyAddr == "" {
			http.Error(w, "nessun proxy VPN configurato — la sessione andrebbe autorizzata sull'IP sbagliato (passa \"direct\":true per una sessione volutamente senza proxy)", http.StatusPreconditionFailed)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	id, err := c.StartSession(ctx, body.URL, proxyAddr)
	if err != nil {
		log.Printf("[vpn/session] start %s: %v", body.URL, err)
		http.Error(w, "avvio sessione fallito: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "target_url": body.URL})
}

// POST /admin/vpn/session/{id}/close → {ok} — persists the session cookies.
func closeVPNSession(w http.ResponseWriter, r *http.Request) {
	closeVPNSessionImpl(w, r, true)
}

// POST /admin/vpn/session/{id}/discard → {ok} — closes WITHOUT persisting the
// session cookies, for a session that was just a look-around (a tab that
// doesn't need authorizing) whose cookies would only pollute cobweb's jar.
func discardVPNSession(w http.ResponseWriter, r *http.Request) {
	closeVPNSessionImpl(w, r, false)
}

func closeVPNSessionImpl(w http.ResponseWriter, r *http.Request, saveCookies bool) {
	c := requireCobweb(w)
	if c == nil {
		return
	}
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := c.CloseSession(ctx, id, saveCookies); err != nil {
		log.Printf("[vpn/session] close %s (save=%v): %v", id, saveCookies, err)
		http.Error(w, "chiusura sessione fallita: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

var vpnSessionUpgrader = websocket.Upgrader{
	// The admin dashboard and this endpoint are same-origin (both served by
	// mycelium); CheckOrigin stays default-permissive here only because the
	// route is already behind adminAuthMiddleware — the auth cookie, not
	// Origin, is what actually gates access.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// GET /admin/vpn/session/{id}/vnc-ws — proxies the admin's websocket
// connection through to cobweb's own vnc-ws endpoint for the same session
// id. Double-bridge (admin↔mycelium↔cobweb) so the admin's browser never
// connects to cobweb directly.
func vpnSessionVNCWS(w http.ResponseWriter, r *http.Request) {
	c := requireCobweb(w)
	if c == nil {
		return
	}
	id := r.PathValue("id")

	upstreamURL := strings.Replace(c.BaseURL(), "http://", "ws://", 1)
	upstreamURL = strings.Replace(upstreamURL, "https://", "wss://", 1)
	upstreamURL += "/v1/session/" + id + "/vnc-ws"

	dialer := *websocket.DefaultDialer
	dialer.NetDialContext = dockerAwareDialContext()
	upstream, _, err := dialer.Dial(upstreamURL, nil)
	if err != nil {
		log.Printf("[vpn/session] vnc-ws %s: dial cobweb: %v", id, err)
		http.Error(w, "sessione non raggiungibile su cobweb", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	client, err := vpnSessionUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[vpn/session] vnc-ws %s: upgrade: %v", id, err)
		return
	}
	defer client.Close()

	errc := make(chan error, 2)
	pipe := func(dst, src *websocket.Conn) {
		for {
			mt, data, err := src.ReadMessage()
			if err != nil {
				errc <- err
				return
			}
			if err := dst.WriteMessage(mt, data); err != nil {
				errc <- err
				return
			}
		}
	}
	core.SafeGo("api/vnc-ws-pipe-up", func() { pipe(upstream, client) })
	core.SafeGo("api/vnc-ws-pipe-down", func() { pipe(client, upstream) })

	err = <-errc
	if err != nil && err != io.EOF {
		log.Printf("[vpn/session] vnc-ws %s: closed: %v", id, err)
	}
}

// vpnNoVNCProxy reverse-proxies /admin/vpn/novnc/* to cobweb's /vnc/* — the
// noVNC static web client (vendored into cobweb's image, see its
// Dockerfile). Built lazily against managers.BrowserClient's current base
// URL rather than a fixed target at startup, consistent with how the rest
// of this file reads it per-request (cobweb's address is only known once
// ConnectBrowserClient runs in main.go).
func vpnNoVNCStatic(w http.ResponseWriter, r *http.Request) {
	c := requireCobweb(w)
	if c == nil {
		return
	}
	target, err := url.Parse(c.BaseURL())
	if err != nil {
		http.Error(w, "indirizzo cobweb non valido", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	if dial := dockerAwareDialContext(); dial != nil {
		proxy.Transport = &http.Transport{DialContext: dial}
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = strings.Replace(r.URL.Path, "/admin/vpn/novnc/", "/vnc/", 1)
	proxy.ServeHTTP(w, r2)
}

// readJSONBody/writeJSON are tiny local helpers — internal/api doesn't
// already have shared ones (handlers decode/encode inline throughout this
// package).
func readJSONBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
