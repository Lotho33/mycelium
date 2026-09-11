package api

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// InternalRoutes registers non-Stremio utility endpoints: proxy, progress, version.
func InternalRoutes(mux *http.ServeMux) {
	rl := func(h http.HandlerFunc) http.HandlerFunc {
		return rateLimitMiddleware(apiLimiter, clientAuthMiddleware(h))
	}
	mux.HandleFunc("GET /api/version", getVersion)
	mux.HandleFunc("GET /api/v1/version", getVersion)
	mux.HandleFunc("POST /api/v1/progress", rl(postProgress))
	mux.HandleFunc("DELETE /api/v1/progress/{provider_id}/{playable_id}", rl(deleteProgress))
	mux.HandleFunc("GET /api/v1/continue_watching", rl(getContinueWatching))

	// HLS proxy — no auth (called directly by media players); the URLs carry an
	// HMAC minted by the resolver so they can't be forged into an open relay.
	// Per-IP rate limit on top.
	mux.HandleFunc("GET /proxy/playlist.m3u8", rateLimitMiddleware(proxyLimiter, ProxyPlaylist))
	mux.HandleFunc("GET /proxy/segment.ts", rateLimitMiddleware(proxyLimiter, ProxySegment))
	mux.HandleFunc("GET /proxy/key.key", rateLimitMiddleware(proxyLimiter, ProxyKey))

	// Poster/fanart thumbnail proxy — no auth (images aren't secret; the
	// client fetches them without a token, same as /plugin-icon). Fails open.
	// SSRF-guarded dialer + per-IP rate limit.
	mux.HandleFunc("GET /img", rateLimitMiddleware(imgLimiter, ImageProxy))
}

// SetupRoutes registra le rotte del setup
func SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup", setupUI)
	mux.HandleFunc("POST /setup/save", saveSetup)
	// Public endpoint — Pileus uses this to confirm the server is Mycelium.
	mux.HandleFunc("GET /pileus/info", pileusInfo)
	// Public — plugin branding icon for the Pileus side menu (manifest `icon:`).
	mux.HandleFunc("GET /plugin-icon/{plugin_id}", pluginIcon)
	// PWA install guide — no auth required (opened in Safari by the user)
	mux.HandleFunc("GET /install", installUI)
	mux.HandleFunc("GET /manifest.json", serveManifest)

	// Pileus web app — Flutter web build served as PWA.
	//   /app/     ← data/pileus-web/      (mobile build; the PWA/install target)
	//   /app-tv/  ← data/pileus-web-tv/   (TV build; optional)
	// Drop the output of `flutter build web` into the matching directory.
	serveWebApp(mux, "/app", core.AppPath("data", "pileus-web"))
	serveWebApp(mux, "/app-tv", core.AppPath("data", "pileus-web-tv"))
}

// serveWebApp wires prefix ("/app") + prefix+"/" as an SPA static server rooted
// at dir, serving index.html for unknown paths (client-side routing).
func serveWebApp(mux *http.ServeMux, prefix, dir string) {
	mux.HandleFunc("GET "+prefix, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET "+prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if path == "" || path == "/" {
			path = "/index.html"
		}
		// Serve index.html for unknown paths (SPA routing).
		fullPath := filepath.Join(dir, filepath.Clean(path))
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			fullPath = filepath.Join(dir, "index.html")
		}
		// http.ServeFile with the *original* request (not a path-rewritten
		// clone): Go's serveFile 301-redirects any request whose URL.Path ends
		// in "index.html" to "./", which the browser resolves back against the
		// current URL — an infinite loop if we rewrote the path to the bare
		// "/index.html". r.URL.Path is left untouched here so that never fires.
		http.ServeFile(w, r, fullPath)
	})
}

func installUI(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles(filepath.Join(core.BasePath, "web", "templates", "install.html"))
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	// Execute can fail mid-write (e.g. a bad field reference) after headers are
	// already sent, when http.Error is no longer an option — log it so a
	// truncated page shows up somewhere instead of silently.
	if err := tmpl.Execute(w, nil); err != nil {
		log.Printf("[install] template execute: %v", err)
	}
}

func serveManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	http.ServeFile(w, r, filepath.Join(core.BasePath, "web", "static", "manifest.json"))
}

// pileusInfo serves the same payload as the UDP discovery responder
// (discovery.go), for a client that already knows the IP (typed in by hand,
// or a repeat connection) and just wants to confirm this is Mycelium and
// read its gRPC TLS fingerprint — SHA-256 (hex) of the server's self-signed
// cert, empty when TLS is off. The cert has no public CA behind it (see
// pileus.GenerateOrLoadTLSCert) — the client is expected to pin this value
// on first contact (TOFU) rather than validate a cert chain.
func pileusInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(discoveryInfo())
}

func setupUI(w http.ResponseWriter, r *http.Request) {
	if managers.Settings.IsSetupDone() {
		http.Redirect(w, r, "/admin/dashboard", http.StatusFound)
		return
	}

	tmpl, err := template.ParseFiles(filepath.Join(core.BasePath, "web", "templates", "setup.html"))
	if err != nil {
		http.Error(w, "Errore caricamento template", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"HasAdminPassword": false,
	}
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("[setup] template execute: %v", err)
	}
}

func saveSetup(w http.ResponseWriter, r *http.Request) {
	// Il setup è un'operazione one-shot: senza questo controllo, POST
	// /setup/save restava raggiungibile senza autenticazione per tutta la
	// vita del processo (non solo durante il first-run) e chiunque poteva
	// riscrivere master_admin_hash a piacere — furto completo dell'account
	// admin. setupUI (sopra) reindirizzava già in questo caso, ma la route
	// POST non replicava lo stesso controllo.
	if managers.Settings.IsSetupDone() {
		http.Error(w, "Setup già completato", http.StatusForbidden)
		return
	}

	r.ParseMultipartForm(1 << 20)
	adminPass := r.FormValue("master_admin_password")

	if len(adminPass) < 8 {
		http.Error(w, "La password deve essere di almeno 8 caratteri", http.StatusBadRequest)
		return
	}

	hashedPassword, err := core.GetPasswordHash(adminPass)
	if err != nil {
		http.Error(w, "Errore generazione hash", http.StatusInternalServerError)
		return
	}
	if err := managers.Settings.Save(map[string]any{
		"master_admin_hash": hashedPassword,
	}); err != nil {
		http.Error(w, "Errore salvataggio configurazione", http.StatusInternalServerError)
		return
	}

	// Autentica subito il browser che ha appena creato l'account, così può
	// proseguire su /admin/dashboard senza un secondo login.
	setAdminSessionCookie(w, r)

	http.Redirect(w, r, "/admin/dashboard", http.StatusSeeOther)
}
