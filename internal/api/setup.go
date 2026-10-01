package api

import (
	"encoding/json"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// InternalRoutes registers the utility endpoints: proxy, progress, version.
func InternalRoutes(mux *http.ServeMux) {
	rl := func(h http.HandlerFunc) http.HandlerFunc {
		return rateLimitMiddleware(apiLimiter, clientAuthMiddleware(h))
	}
	mux.HandleFunc("GET /api/version", getVersion)
	mux.HandleFunc("GET /api/v1/version", getVersion)
	mux.HandleFunc("POST /api/v1/progress", rl(postProgress))
	mux.HandleFunc("DELETE /api/v1/progress/{provider_id}/{playable_id}", rl(deleteProgress))
	mux.HandleFunc("GET /api/v1/continue_watching", rl(getContinueWatching))

	// HLS proxy: no auth (players call it directly); the URLs carry an HMAC so
	// they can't be forged. Per-IP rate limit on top.
	mux.HandleFunc("GET /proxy/playlist.m3u8", rateLimitMiddleware(proxyLimiter, ProxyPlaylist))
	mux.HandleFunc("GET /proxy/segment.ts", rateLimitMiddleware(proxyLimiter, ProxySegment))
	mux.HandleFunc("GET /proxy/key.key", rateLimitMiddleware(proxyLimiter, ProxyKey))

	// Image proxy: no auth, SSRF-guarded, per-IP rate limit.
	mux.HandleFunc("GET /img", rateLimitMiddleware(imgLimiter, ImageProxy))
}

// SetupRoutes registers the setup and public endpoints.
func SetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup", setupUI)
	mux.HandleFunc("POST /setup/save", saveSetup)
	// Public: lets a client confirm this is mycelium and read the gRPC TLS
	// fingerprint.
	mux.HandleFunc("GET /pileus/info", pileusInfo)
	// Public: a plugin's branding icon (manifest `icon:`).
	mux.HandleFunc("GET /plugin-icon/{plugin_id}", pluginIcon)
	// Web-app install guide.
	mux.HandleFunc("GET /install", installUI)
	mux.HandleFunc("GET /manifest.json", serveManifest)

	// Pileus web app (a Flutter web build, served as a PWA):
	//   /app/     ← data/pileus-web/
	//   /app-tv/  ← data/pileus-web-tv/ (optional TV build)
	serveWebApp(mux, "/app", core.AppPath("data", "pileus-web"))
	serveWebApp(mux, "/app-tv", core.AppPath("data", "pileus-web-tv"))
}

// baseHrefRe matches the <base href="..."> tag of a Flutter build.
var baseHrefRe = regexp.MustCompile(`(?i)<base\s+href\s*=\s*(?:"[^"]*"|'[^']*')`)

// serveWebApp serves dir at prefix as a single-page app: unknown paths get
// index.html.
func serveWebApp(mux *http.ServeMux, prefix, dir string) {
	indexPath := filepath.Join(dir, "index.html")
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
			fullPath = indexPath
		}
		if fullPath == indexPath {
			// The build's <base href="/"> is rewritten to this mount prefix, so the
			// same bundle works under any path.
			serveIndexHTML(w, r, indexPath, prefix)
			return
		}
		// Serve with the original request: http.ServeFile redirects paths ending in
		// "index.html", which would loop on a rewritten path.
		http.ServeFile(w, r, fullPath)
	})
}

// serveIndexHTML serves indexPath with its <base href> rewritten to prefix.
func serveIndexHTML(w http.ResponseWriter, r *http.Request, indexPath, prefix string) {
	data, err := os.ReadFile(indexPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data = baseHrefRe.ReplaceAll(data, []byte(`<base href="`+prefix+`/"`))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// pileusWebVersionInfo is the part of the build's version.json used here.
type pileusWebVersionInfo struct {
	Version     string `json:"version"`
	BuildNumber string `json:"build_number"`
}

// pileusWebVersion reads the installed build's version.json; ok is false
// when there is no build or the file doesn't parse.
func pileusWebVersion() (info pileusWebVersionInfo, ok bool) {
	data, err := os.ReadFile(core.AppPath("data", "pileus-web", "version.json"))
	if err != nil {
		return pileusWebVersionInfo{}, false
	}
	if err := json.Unmarshal(data, &info); err != nil || info.Version == "" {
		return pileusWebVersionInfo{}, false
	}
	return info, true
}

func installUI(w http.ResponseWriter, r *http.Request) {
	tmpl, err := template.ParseFiles(filepath.Join(core.BasePath, "web", "templates", "install.html"))
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	data := map[string]any{}
	if info, ok := pileusWebVersion(); ok {
		data["PileusVersion"] = info.Version
		data["PileusBuild"] = info.BuildNumber
	}
	// Headers are already sent: log a failed Execute.
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("[install] template execute: %v", err)
	}
}

func serveManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	http.ServeFile(w, r, filepath.Join(core.BasePath, "web", "static", "manifest.json"))
}

// pileusInfo serves the same payload as the UDP discovery responder, for a
// client that already knows the address: a confirmation that this is
// mycelium and the gRPC TLS fingerprint to pin on first contact ("" when TLS
// is off).
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

// setupAllowAny lifts the local-network restriction on POST /setup/save.
var setupAllowAny = os.Getenv("MYCELIUM_SETUP_ALLOW_ANY") == "1"

// setupCGNAT is 100.64.0.0/10 (Tailscale/Headscale peers count as local).
var setupCGNAT = &net.IPNet{IP: net.IPv4(100, 64, 0, 0).To4(), Mask: net.CIDRMask(10, 32)}

// setupAllowedFrom reports whether a first-run setup may come from ip:
// loopback, RFC1918/ULA, link-local or the tailnet range.
func setupAllowedFrom(ip string) bool {
	if setupAllowAny {
		return true
	}
	// Link-local IPv6 addresses carry a zone ("fe80::1%eth0") ParseIP rejects.
	if i := strings.IndexByte(ip, '%'); i >= 0 {
		ip = ip[:i]
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	return parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast() || setupCGNAT.Contains(parsed)
}

func saveSetup(w http.ResponseWriter, r *http.Request) {
	// One-shot: once an admin password exists this endpoint is closed.
	if managers.Settings.IsSetupDone() {
		http.Error(w, "Setup già completato", http.StatusForbidden)
		return
	}
	// Until then it is unauthenticated (first boot, or after a factory reset):
	// accept it only from the local network.
	if !setupAllowedFrom(realIP(r)) {
		log.Printf("[setup] rifiutato setup da %s (non in rete locale; MYCELIUM_SETUP_ALLOW_ANY=1 per consentirlo)", realIP(r))
		http.Error(w, "Il setup iniziale è consentito solo dalla rete locale", http.StatusForbidden)
		return
	}

	r.ParseMultipartForm(1 << 20)
	adminPass := r.FormValue("master_admin_password")
	adminPassConfirm := r.FormValue("master_admin_password_confirm")

	if len(adminPass) < 8 {
		http.Error(w, "La password deve essere di almeno 8 caratteri", http.StatusBadRequest)
		return
	}
	// The form checks this too; the server must as well.
	if adminPass != adminPassConfirm {
		http.Error(w, "Le due password non coincidono", http.StatusBadRequest)
		return
	}

	hashedPassword, err := core.GetPasswordHash(adminPass)
	if err != nil {
		http.Error(w, "Errore generazione hash", http.StatusInternalServerError)
		return
	}
	// SaveInternal: master_admin_hash is not writable through Save.
	if err := managers.Settings.SaveInternal(map[string]any{
		"master_admin_hash": hashedPassword,
	}); err != nil {
		http.Error(w, "Errore salvataggio configurazione", http.StatusInternalServerError)
		return
	}

	// Log the browser that just created the account straight in.
	setAdminSessionCookie(w, r)

	http.Redirect(w, r, "/admin/dashboard", http.StatusSeeOther)
}
