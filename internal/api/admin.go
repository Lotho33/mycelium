package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// --- SESSIONI ADMIN ---
//
// Self-verifying cookie (id.expiry.hmac) instead of an opaque token looked up
// in an in-memory map: the old map was wiped on every process restart, and a
// self-update triggered from the dashboard restarts the process — the admin
// got logged out mid-action every time (P2-6). Nothing server-side needs to
// remember a session to validate it, so a restart no longer invalidates one.
// Explicit logout (revokeAdminSession) is the one thing that still needs
// server-side state — a denylist of not-yet-expired ids, small and fine to
// lose on restart: the narrow, low-severity edge case is an admin who logs
// out right as the process restarts keeping a technically-still-valid cookie
// until its natural 12h expiry, instead of it dying immediately.

const adminSessionCookie = "fgb_admin_session"
const adminSessionTTL = 12 * time.Hour

// adminSessionKey signs the cookie; nil until SetAdminSessionKey runs at
// startup (cmd/server/main.go, derived from the same master secret as the
// Pileus JWT / the /proxy URL signature — a different subkey per use, no
// shared key material across the three).
var adminSessionKey []byte

// SetAdminSessionKey installs the signing key, derived from master. Call once
// at startup.
func SetAdminSessionKey(master []byte) {
	if len(master) == 0 {
		adminSessionKey = nil
		return
	}
	m := hmac.New(sha256.New, master)
	m.Write([]byte("mycelium/admin-session/v1"))
	adminSessionKey = m.Sum(nil)
}

func generateSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand non disponibile: " + err.Error())
	}
	return base64.URLEncoding.EncodeToString(b)
}

func signAdminSession(id string, exp int64) string {
	mac := hmac.New(sha256.New, adminSessionKey)
	fmt.Fprintf(mac, "%s.%d", id, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// createAdminSession mints a new self-verifying token: <random-id>.<exp>.<hmac>.
func createAdminSession() string {
	id := generateSessionToken()
	exp := time.Now().Add(adminSessionTTL).Unix()
	return fmt.Sprintf("%s.%d.%s", id, exp, signAdminSession(id, exp))
}

func isValidAdminSession(token string) bool {
	if len(adminSessionKey) == 0 {
		return false // never initialised — fail closed rather than accept anything
	}
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return false
	}
	id, expStr, sig := parts[0], parts[1], parts[2]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(signAdminSession(id, exp)), []byte(sig)) != 1 {
		return false
	}
	if time.Now().Unix() >= exp {
		return false
	}
	return !isRevokedAdminSession(id)
}

// revokedAdminSessions denylists a session id (not the whole token — no need
// to keep the signature around) from an explicit logout until it would have
// expired naturally anyway.
var (
	revokedAdminSessions   = make(map[string]int64) // id -> expiry (unix)
	revokedAdminSessionsMu sync.Mutex
)

func isRevokedAdminSession(id string) bool {
	revokedAdminSessionsMu.Lock()
	defer revokedAdminSessionsMu.Unlock()
	_, revoked := revokedAdminSessions[id]
	return revoked
}

func init() {
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			func() {
				defer core.Guard("api/admin-session-gc-tick")
				now := time.Now().Unix()
				revokedAdminSessionsMu.Lock()
				defer revokedAdminSessionsMu.Unlock()
				for id, exp := range revokedAdminSessions {
					if now >= exp {
						delete(revokedAdminSessions, id)
					}
				}
			}()
		}
	}()
}

func revokeAdminSession(token string) {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return
	}
	id := parts[0]
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		exp = time.Now().Add(adminSessionTTL).Unix() // fallback: denylist for a full TTL
	}
	revokedAdminSessionsMu.Lock()
	revokedAdminSessions[id] = exp
	revokedAdminSessionsMu.Unlock()
}

// serverStartTime tracks when the server process started, for uptime display.
var serverStartTime = time.Now()

// adminAuthMiddleware protegge le route /admin/* richiedendo una sessione valida.
// Richieste API (Accept: application/json o text/event-stream) ricevono 401; le altre vengono reindirizzate al login.
func adminAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(adminSessionCookie)
		if err != nil || !isValidAdminSession(cookie.Value) {
			accept := r.Header.Get("Accept")
			if strings.Contains(accept, "application/json") ||
				strings.Contains(accept, "text/event-stream") ||
				r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
				http.Error(w, `{"detail":"non autorizzato"}`, http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// --- SISTEMA DI LOG IN MEMORIA ---
type LogBuffer struct {
	lines   []string
	total   int64 // monotonic append count — never wraps in practice
	mu      sync.RWMutex
	max     int
	fileOut io.Writer // optional file sink (set via SetFileOutput)
}

var UILogs = &LogBuffer{max: 300}

// SetFileOutput attaches a file writer; all subsequent log lines are also written there.
func (l *LogBuffer) SetFileOutput(w io.Writer) {
	l.mu.Lock()
	l.fileOut = w
	l.mu.Unlock()
}

func (l *LogBuffer) Write(p []byte) (n int, err error) {
	line := strings.TrimSpace(string(p))
	if line == "" {
		return len(p), nil
	}
	if len(line) == 0 || line[0] < '0' || line[0] > '9' {
		line = time.Now().Format("2006/01/02 15:04:05") + " " + line
	}
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.total++
	if len(l.lines) > l.max {
		l.lines = l.lines[1:]
	}
	fw := l.fileOut
	l.mu.Unlock()
	if fw != nil {
		fw.Write([]byte(line + "\n"))
	}
	return os.Stdout.Write(p)
}

func (l *LogBuffer) Append(line string) {
	if line == "" {
		return
	}
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.total++
	if len(l.lines) > l.max {
		l.lines = l.lines[1:]
	}
	l.mu.Unlock()
}

// snapshot returns a copy of current lines and the monotonic total append count.
func (l *LogBuffer) snapshot() (lines []string, total int64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]string(nil), l.lines...), l.total
}

func AdminRoutes(mux *http.ServeMux) {
	// Login/logout — pubblici (rate limited sul POST)
	mux.HandleFunc("GET /admin/login", adminLoginPage)
	mux.HandleFunc("POST /admin/login", rateLimitMiddleware(loginLimiter, adminLoginSubmit))
	mux.HandleFunc("POST /admin/logout", adminLogout)

	// Tutto il resto richiede sessione valida
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/dashboard", auth(adminDashboard))
	mux.HandleFunc("GET /admin/logs", auth(getLogs))

	mux.HandleFunc("GET /admin/clients", auth(listClients))
	mux.HandleFunc("POST /admin/clients", auth(addClient))
	mux.HandleFunc("DELETE /admin/clients/{client_id}", auth(removeClient))
	mux.HandleFunc("GET /admin/dev-token", auth(generateDevToken))

	// Lua plugin management lives under /admin/lua-plugins/* (RegisterLuaAdminRoutes).

	mux.HandleFunc("POST /admin/settings/save", auth(saveSettings))
	mux.HandleFunc("POST /admin/cache/clear", auth(clearCacheHandler))
	mux.HandleFunc("POST /admin/profiles/wipe", auth(wipeProfilesHandler))
	mux.HandleFunc("POST /admin/plugins/wipe-data", auth(wipePluginDataHandler))
	mux.HandleFunc("GET /admin/core/resources", auth(getCoreResources))
	mux.HandleFunc("GET /admin/plugins/info", auth(getPluginsInfo))
	mux.HandleFunc("GET /admin/enricher-bindings", auth(getEnricherBindings))
	mux.HandleFunc("GET /admin/info", auth(getAdminInfo))
	mux.HandleFunc("GET /admin/system/status", auth(getSystemStatus))
	// Generic proxy URL for plugin egress (VPN/WARP/whatever the operator
	// wires up). Not surfaced in the dashboard for now — VPN_PROXY_URL in the
	// environment is the primary knob; a per-operator config UI comes later.
	mux.HandleFunc("POST /admin/vpn", auth(setVPNProxy))
	mux.HandleFunc("POST /admin/vpn/test", auth(testVPNConnection))
	mux.HandleFunc("GET /admin/logs/stream", auth(getLogsSSE))
	mux.HandleFunc("POST /admin/core/update", auth(coreUpdate))

	// Lua plugin management
	RegisterLuaAdminRoutes(mux)

	// Interactive browser session (noVNC) — complete an upstream verification by hand
	RegisterVPNSessionRoutes(mux)

	// Egress registry — per-plugin choice of network exit (direct / warp / …)
	RegisterEgressRoutes(mux)

	// Pileus device pairing — rotating code instead of the admin password
	RegisterPairingRoutes(mux)

	// Pileus device management — list / revoke / restore a paired device
	RegisterPileusDeviceRoutes(mux)
}

func adminLoginPage(w http.ResponseWriter, r *http.Request) {
	if managers.Settings.IsSetupDone() {
		if c, err := r.Cookie(adminSessionCookie); err == nil && isValidAdminSession(c.Value) {
			http.Redirect(w, r, "/admin/dashboard", http.StatusSeeOther)
			return
		}
	} else {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, loginHTML(false))
}

// setAdminSessionCookie creates a new admin session and attaches it to the
// response — shared by the login form and saveSetup (which auto-logs-in the
// browser that just created the admin account, so it can immediately call
// other /admin/* endpoints, e.g. POST /admin/network/tailscale during the
// setup wizard's network step).
func setAdminSessionCookie(w http.ResponseWriter, r *http.Request) {
	token := createAdminSession()
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   43200, // 12h
	})
}

func adminLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminSessionCookie); err == nil && isValidAdminSession(c.Value) {
		http.Redirect(w, r, "/admin/dashboard", http.StatusSeeOther)
		return
	}
	r.ParseForm()
	password := r.FormValue("password")
	if err := core.VerifyAdmin(password, managers.Settings.MasterAdminHash()); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, loginHTML(true))
		return
	}
	setAdminSessionCookie(w, r)
	http.Redirect(w, r, "/admin/dashboard", http.StatusSeeOther)
}

func adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminSessionCookie); err == nil {
		revokeAdminSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   adminSessionCookie,
		Value:  "",
		Path:   "/admin",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// loginHTML renders the admin login page — mobile-first, matching the
// dashboard/setup visual language (dark, purple hexagon mark, safe-area).
// withErr adds the "wrong password" line.
func loginHTML(withErr bool) string {
	errLine := ""
	if withErr {
		errLine = `<p style="color:#f87171;font-size:.8rem;margin:0 0 .75rem">Password errata.</p>`
	}
	return `<!DOCTYPE html><html lang="it"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="theme-color" content="#0a0d12">
<meta name="apple-mobile-web-app-capable" content="yes">
<title>Mycelium — Accesso</title>
<style>
  *{box-sizing:border-box}
  body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
       padding:calc(env(safe-area-inset-top) + 1.5rem) 1.25rem calc(env(safe-area-inset-bottom) + 1.5rem);
       background:#0a0d12;color:#e5e7eb;
       font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;
       -webkit-tap-highlight-color:transparent}
  .box{width:100%;max-width:22rem;background:#141820;border:1px solid #1e2530;
       border-radius:1rem;padding:1.75rem}
  .mark{width:3rem;height:3rem;border-radius:.85rem;display:flex;align-items:center;justify-content:center;
        margin:0 auto .9rem;background:linear-gradient(135deg,#7c6af7,#5a4fd4)}
  h1{font-size:1.15rem;font-weight:700;color:#fff;text-align:center;margin:0 0 .25rem}
  p.sub{font-size:.8rem;color:#6b7280;text-align:center;margin:0 0 1.4rem}
  label{display:block;font-size:.75rem;color:#9ca3af;margin-bottom:.4rem}
  input{width:100%;padding:.8rem .9rem;font-size:1rem;border-radius:.6rem;
        border:1px solid #2a3040;background:#0d1017;color:#fff;outline:none}
  input:focus{border-color:#7c6af7;box-shadow:0 0 0 3px rgba(124,106,247,.15)}
  button{width:100%;margin-top:1rem;padding:.85rem;font-size:1rem;font-weight:700;
         border:0;border-radius:.6rem;background:#7c6af7;color:#fff;cursor:pointer}
  button:active{background:#6a58e8}
</style></head>
<body><div class="box">
  <div class="mark"><svg viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2"
       stroke-linecap="round" stroke-linejoin="round" width="26" height="26">
       <path d="M12 2L2 7l10 5 10-5-10-5z"/><path d="M2 17l10 5 10-5"/><path d="M2 12l10 5 10-5"/></svg></div>
  <h1>Mycelium</h1>
  <p class="sub">Accesso amministratore</p>
  <form method="POST" action="/admin/login">
    ` + errLine + `
    <label for="pw">Password</label>
    <input id="pw" type="password" name="password" autocomplete="current-password"
           autofocus required>
    <button type="submit">Accedi</button>
  </form>
</div></body></html>`
}

func adminDashboard(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles(filepath.Join(core.BasePath, "web", "templates", "admin.html"))
	if err != nil {
		http.Error(w, "Template non trovato", http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, nil); err != nil {
		log.Printf("[admin] template execute: %v", err)
	}
}

func getLogs(w http.ResponseWriter, r *http.Request) {
	UILogs.mu.RLock()
	defer UILogs.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"logs": UILogs.lines})
}

func generateSecretKey() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand non disponibile: " + err.Error())
	}
	return base64.URLEncoding.EncodeToString(b)
}

// listClients godoc
//
//	@Summary		Lista tutti i client
//	@Description	Recupera l'elenco dei dispositivi autorizzati dal database
//	@Tags			Admin
//	@Produce		json
//	@Success		200	{array}	map[string]string
//	@Router			/admin/clients [get]
func listClients(w http.ResponseWriter, r *http.Request) {
	secrets, err := managers.DB.GetAllClientSecrets()
	if err != nil {
		http.Error(w, "Errore caricamento client", 500)
		return
	}
	type clientEntry struct {
		ClientID string `json:"client_id"`
	}
	result := make([]clientEntry, 0, len(secrets))
	for id := range secrets {
		result = append(result, clientEntry{ClientID: id})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// addClient godoc
//
//	@Summary		Crea nuovo client
//	@Description	Genera un nuovo client con una secret key casuale
//	@Tags			Admin
//	@Param			client_id	query	string	true	"ID univoco del client"
//	@Produce		json
//	@Success		200	{object}	map[string]string
//	@Failure		400	{object}	map[string]string
//	@Router			/admin/clients [post]
func addClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		http.Error(w, "ID mancante", 400)
		return
	}

	newSecret := generateSecretKey()

	err := managers.DB.AddClient(clientID, newSecret)
	if err != nil {
		http.Error(w, "Errore salvataggio: ID già esistente?", 400)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"client_id":  clientID,
		"SECRET_KEY": newSecret,
	})
}

// removeClient godoc
//
//	@Summary		Rimuovi client
//	@Description	Elimina un client autorizzato dal database
//	@Tags			Admin
//	@Param			client_id	path	string	true	"ID del client da rimuovere"
//	@Produce		json
//	@Success		200	{object}	map[string]string
//	@Failure		500	{object}	map[string]string
//	@Router			/admin/clients/{client_id} [delete]
func removeClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("client_id")

	w.Header().Set("Content-Type", "application/json")
	if err := managers.DB.RemoveClient(clientID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"detail": "Errore DB"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

// generateDevToken godoc
//
//	@Summary		Genera token di sviluppo
//	@Description	Crea un token X-Auth-Token HMAC valido 5 minuti per testare le API
//	@Tags			Admin
//	@Param			client_id	query	string	true	"ID del client"
//	@Produce		json
//	@Success		200	{object}	map[string]string
//	@Failure		404	{object}	map[string]string
//	@Router			/admin/dev-token [get]
func generateDevToken(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	secret, err := managers.DB.GetClientSecret(clientID)
	if err != nil {
		http.Error(w, "Client non trovato", http.StatusNotFound)
		return
	}

	timestamp := time.Now().Unix()
	message := fmt.Sprintf("%s:%d", clientID, timestamp)

	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(message))
	signature := hex.EncodeToString(h.Sum(nil))

	token := fmt.Sprintf("%s:%d:%s", clientID, timestamp, signature)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"X-Auth-Token": token})
}

func saveSettings(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Body non valido", http.StatusBadRequest)
		return
	}
	if err := managers.Settings.Save(payload); err != nil {
		http.Error(w, "Errore salvataggio", http.StatusInternalServerError)
		return
	}
	if _, ok := payload["http_profile"]; ok {
		// Rebuild the HLS-proxy upstream clients so the new profile applies
		// without a restart.
		resetUpstreamClients()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func getCoreResources(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(managers.GetCoreStats())
}

// getPluginsInfo returns per-plugin config schema + lifecycle state for the
// dashboard's plugin panel. All plugins are Lua; settings come from
// managers.Settings, lifecycle from the engine.
func getPluginsInfo(w http.ResponseWriter, r *http.Request) {
	result := map[string]any{}

	for _, mf := range engine.LuaPlugins.GetMeta() {
		runState := engine.LuaPlugins.RunStateOf(mf.ID)
		missingRequired := engine.LuaPlugins.MissingRequiredSettings(mf.ID)
		isActive := runState == engine.RunStateRunning

		type luaField struct {
			Key          string `json:"key"`
			Label        string `json:"label"`
			Type         string `json:"type"`
			Required     bool   `json:"required"`
			CurrentValue string `json:"current_value,omitempty"`
			IsSet        bool   `json:"is_set,omitempty"`
		}
		type luaTask struct {
			Function string `json:"function"`
			Cron     string `json:"cron"`
		}
		fields := make([]luaField, 0, len(mf.Settings.Global))
		for _, f := range mf.Settings.Global {
			val := managers.Settings.GetString("lua:"+mf.ID+":global:"+f.ID, "")
			lf := luaField{Key: f.ID, Label: f.Label, Type: f.Type, Required: f.Required}
			if f.Type == "password" {
				lf.IsSet = val != ""
			} else {
				lf.CurrentValue = val
			}
			fields = append(fields, lf)
		}

		tasks := make([]luaTask, 0, len(mf.Tasks))
		for _, t := range mf.Tasks {
			tasks = append(tasks, luaTask{Function: t.Function, Cron: t.Cron})
		}

		status := map[string]string{
			engine.RunStateRunning: "ok",
			engine.RunStateStopped: "disabled",
			engine.RunStateWaiting: "not_configured",
		}[runState]

		catalogs := mf.Exposes.Catalogs
		runtimeStatus := engine.LuaPlugins.GetStatus(mf.ID)
		result[mf.ID] = map[string]any{
			"plugin_name":      mf.Name,
			"plugin_type":      "lua",
			"description":      mf.Description,
			"status":           status,
			"is_active":        isActive,
			"run_state":        runState,
			"missing_required": missingRequired,
			"fields":           fields,
			"tasks":            tasks,
			"process":          true,
			"ready":            engine.LuaPlugins.Has(mf.ID),
			"catalogs_total":   len(catalogs),
			"catalogs_enabled": len(catalogs),
			"capabilities":     mf.Exposes.Capabilities,
			"egress":           engine.LuaPlugins.PluginEgress(mf.ID),
			"runtime_status":   map[string]string{"label": runtimeStatus.Label, "detail": runtimeStatus.Detail},
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// getEnricherBindings is a compatibility stub — enricher plugins were part of
// the removed gRPC plugin system. The dashboard still calls it and tolerates
// an empty result.
func getEnricherBindings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"bindings": map[string]any{}, "providers": []any{}})
}

// getSystemStatus godoc
//
//	@Summary		Stato servizi
//	@Description	Stato di Core, Extractor (cobweb) e Redis con statistiche RAM/CPU dove disponibili
//	@Tags			Admin
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/system/status [get]
func getSystemStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Redis — risorse dall'API Docker (container_name: redis, fisso in
	// entrambi i compose): è un container sidecar, non un sottoprocesso, /proc
	// non basta.
	redisOk := managers.Redis != nil
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	redisStats := managers.GetContainerStats("redis")

	// Extractor: cobweb, un container a parte — CPU/RAM dall'API Docker
	// (stesso meccanismo di redis), readiness/engine dal suo /health.
	cobwebStats := managers.GetContainerStats("cobweb")
	var browserOk bool
	var browserEngine string
	if managers.BrowserClient != nil {
		hCtx, hCancel := context.WithTimeout(r.Context(), 2*time.Second)
		browserOk, browserEngine, _ = managers.BrowserClient.Health(hCtx)
		hCancel()
	}

	json.NewEncoder(w).Encode(map[string]any{
		"redis": map[string]any{
			"ok":                        redisOk,
			"addr":                      redisAddr,
			"container_available":       redisStats.Available,
			"container_cpu_percent":     redisStats.CPUPercent,
			"container_mem_bytes":       redisStats.MemBytes,
			"container_mem_limit_bytes": redisStats.MemLimit,
		},
		"browser": map[string]any{
			"ok":                        browserOk,
			"engine":                    browserEngine,
			"container_available":       cobwebStats.Available,
			"container_cpu_percent":     cobwebStats.CPUPercent,
			"container_mem_bytes":       cobwebStats.MemBytes,
			"container_mem_limit_bytes": cobwebStats.MemLimit,
		},
	})
}

func setVPNProxy(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "body non valido", http.StatusBadRequest)
		return
	}
	engine.LuaPlugins.SetProxyAddr(payload.Addr)
	// Persiste la scelta così sopravvive a un riavvio (vedi cmd/server/main.go,
	// che la rilegge al boot se VPN_PROXY_URL/WARP_SOCKS5_ADDR non sono impostate).
	if err := managers.Settings.Save(map[string]any{"vpn_proxy_url": payload.Addr}); err != nil {
		log.Printf("[vpn] impossibile salvare lo stato del proxy: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":   payload.Addr != "",
		"addr": payload.Addr,
	})
}

// clearCacheHandler godoc
//
//	@Summary		Svuota cache
//	@Description	Invalida tutta la cache in memoria (risultati browse/search)
//	@Tags			Admin
//	@Produce		json
//	@Success		200	{object}	map[string]string
//	@Router			/admin/cache/clear [post]
func clearCacheHandler(w http.ResponseWriter, r *http.Request) {
	managers.Cache.Flush()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func requireAdminPassword(w http.ResponseWriter, r *http.Request) bool {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		http.Error(w, "password richiesta", http.StatusBadRequest)
		return false
	}
	if err := core.VerifyAdmin(body.Password, managers.Settings.MasterAdminHash()); err != nil {
		http.Error(w, "password errata", http.StatusUnauthorized)
		return false
	}
	return true
}

// wipeProfilesHandler godoc
//
//	@Summary		Cancella tutti i profili
//	@Description	DANGER: elimina tutti i profili Pileus, la loro cronologia di visione e i secret per-profilo dei plugin. I device accoppiati restano. Richiede la password admin nel body.
//	@Tags			Admin
//	@Accept			json
//	@Param			body	body	object{password=string}	true	"password admin"
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/profiles/wipe [post]
func wipeProfilesHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdminPassword(w, r) {
		return
	}

	profRes, err := managers.DB.Exec(`DELETE FROM pileus_profiles`)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	histRes, err := managers.DB.Exec(`DELETE FROM watch_history`)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	nProf, _ := profRes.RowsAffected()
	nHist, _ := histRes.RowsAffected()

	var nRedis int
	if managers.Redis != nil {
		nRedis, _ = managers.Redis.DelByPattern(r.Context(), "mycelium:plugin:*:user:*:secrets")
	}

	log.Printf("[admin] wipe profiles: %d profiles, %d history rows, %d per-profile redis keys", nProf, nHist, nRedis)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":           true,
		"profiles":     nProf,
		"history_rows": nHist,
		"redis_keys":   nRedis,
	})
}

// wipePluginDataHandler godoc
//
//	@Summary		Cancella tutti i dati dei plugin
//	@Description	DANGER: FLUSHDB su Redis, svuota la cache immagini WebP e i file cache su disco dei plugin. I plugin restano installati, abilitati e configurati. Richiede la password admin nel body.
//	@Tags			Admin
//	@Accept			json
//	@Param			body	body	object{password=string}	true	"password admin"
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/plugins/wipe-data [post]
func wipePluginDataHandler(w http.ResponseWriter, r *http.Request) {
	if !requireAdminPassword(w, r) {
		return
	}

	var redisErr string
	if managers.Redis != nil {
		if err := managers.Redis.FlushDB(r.Context()); err != nil {
			redisErr = err.Error()
		}
	}

	imgN := clearDirContents(core.AppPath("data", "imgcache"))
	fileN := clearPluginCacheFiles()
	managers.Cache.Flush() // in-memory browse/search cache is plugin-derived too

	log.Printf("[admin] wipe plugin data: redis flush err=%q, %d webp files, %d plugin cache files", redisErr, imgN, fileN)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":                 redisErr == "",
		"redis_error":        redisErr,
		"webp_files":         imgN,
		"plugin_cache_files": fileN,
	})
}

// clearDirContents deletes every regular file directly inside dir (the
// directory itself is kept). Missing dir → 0, no error.
func clearDirContents(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// clearPluginCacheFiles removes the known regenerated cache files from every
// plugins/<id>/ directory — the same set kept out of the image by
// .dockerignore. Plugins repopulate them on the next sync.
func clearPluginCacheFiles() int {
	names := []string{"catalog_cache.json", "logo_cache.json", "fribb_index.json", "avail_cache.json"}
	root := core.AppPath("plugins")
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, name := range names {
			if os.Remove(filepath.Join(root, e.Name(), name)) == nil {
				n++
			}
		}
	}
	return n
}

// getAdminInfo godoc
//
//	@Summary		Info server
//	@Description	Restituisce versione, uptime e ora di avvio del processo core
//	@Tags			Admin
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/info [get]
//
// effectiveServerHost returns the configured server host: the setting wins,
// then the MYCELIUM_SERVER_HOST env fallback. Mirrors media_handler.go.
func effectiveServerHost() string {
	if h := managers.Settings.GetString("server_host", ""); h != "" {
		return h
	}
	return os.Getenv("MYCELIUM_SERVER_HOST")
}

func getAdminInfo(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(serverStartTime)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"uptime_seconds": int64(uptime.Seconds()),
		"version":        core.Version,
		"latest_version": core.LatestVersion(),
		"start_time":     serverStartTime.Format(time.RFC3339),
		// Indirizzo/hostname reale con cui i client raggiungono il server —
		// usato per costruire gli URL del proxy HLS restituiti al player.
		// Vuoto = autodetect (network_mode: host / bare-metal). In bridge mode
		// va impostato (setting o env MYCELIUM_SERVER_HOST).
		"server_host":     effectiveServerHost(),
		"server_host_env": os.Getenv("MYCELIUM_SERVER_HOST") != "",
		"in_docker":       os.Getenv("MYCELIUM_DOCKER") == "1",
		// Upstream HLS-proxy path: "cobweb" (default — relayed through the
		// cobweb sidecar's /v1/fetch) unless MYCELIUM_HTTP_PROFILE=standard
		// pins a plain net/http stack.
		"http_profile": func() string {
			if strings.EqualFold(os.Getenv("MYCELIUM_HTTP_PROFILE"), "standard") {
				return "standard"
			}
			return "cobweb"
		}(),
	})
}

// getLogsSSE godoc
//
//	@Summary		Stream log SSE
//	@Description	Invia i log del core in tempo reale via Server-Sent Events
//	@Tags			Admin
//	@Produce		text/event-stream
//	@Success		200	{string}	string	"stream SSE"
//	@Router			/admin/logs/stream [get]
func getLogsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	existing, lastTotal := UILogs.snapshot()
	for _, line := range existing {
		fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", " "))
	}
	flusher.Flush()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			lines, total := UILogs.snapshot()
			newCount := total - lastTotal
			if newCount > 0 {
				// Take the last newCount lines; some may be evicted if buffer wrapped.
				start := max(len(lines)-int(newCount), 0)
				for _, line := range lines[start:] {
					fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", " "))
				}
			}
			lastTotal = total
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func coreUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	latest := core.LatestVersion()
	if latest == "" {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"detail": "versione GitHub non ancora disponibile, riprovare tra qualche secondo"})
		return
	}
	if latest == core.Version {
		json.NewEncoder(w).Encode(map[string]string{"status": "up_to_date", "version": core.Version})
		return
	}

	// Docker: MYCELIUM_DOCKER è settata dall'immagine.
	if os.Getenv("MYCELIUM_DOCKER") == "1" {
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "manual_required",
			"detail":  "Ambiente Docker: aggiorna con `docker pull` e riavvia il container.",
			"version": latest,
		})
		return
	}

	// Windows: aggiornamento automatico non supportato (binary in uso).
	if runtime.GOOS == "windows" {
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "manual_required",
			"detail":  "Windows: scarica il nuovo binary da GitHub Releases e sostituisci manualmente.",
			"version": latest,
			"url":     "https://github.com/Lotho33/mycelium-core/releases/latest",
		})
		return
	}

	// Linux bare-metal: delega allo script di aggiornamento installato da install.sh.
	updateScript := "/usr/local/bin/mycelium-update"
	if _, err := os.Stat(updateScript); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{
			"detail": "script di aggiornamento non trovato — reinstalla con install.sh",
			"url":    "https://github.com/Lotho33/mycelium-core/releases/latest",
		})
		return
	}

	log.Printf("[core] avvio aggiornamento → %s", latest)
	cmd := exec.Command(updateScript, latest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"detail": "avvio script fallito: " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]string{
		"status":  "updating",
		"version": latest,
		"detail":  "aggiornamento in corso — il servizio si riavvierà a breve",
	})
}
