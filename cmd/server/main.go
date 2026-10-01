package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"mycelium/internal/api"
	"mycelium/internal/core"
	"mycelium/internal/downloads"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
	"mycelium/internal/pileus"
)

// defaultMemLimitBytes is the soft heap cap used when GOMEMLIMIT is unset,
// sized for low-RAM hosts.
const defaultMemLimitBytes = 350 << 20 // 350 MiB

// applyMemoryLimit sets a soft heap limit (the GC collects more eagerly near
// it; it never refuses an allocation). An operator-set GOMEMLIMIT wins.
func applyMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	debug.SetMemoryLimit(defaultMemLimitBytes)
}

// init makes every DNS lookup of the process go to public resolvers over
// IPv4 UDP (net.DefaultResolver and http.DefaultTransport), bypassing a
// possibly broken local resolver.
func init() {
	// Alternate the server per dial: the Go resolver dials again on each retry,
	// so a filtered server doesn't break every lookup.
	dnsServers := [...]string{"1.1.1.1:53", "8.8.8.8:53"}
	var dnsNext atomic.Uint32
	cfResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			server := dnsServers[int(dnsNext.Add(1)-1)%len(dnsServers)]
			return d.DialContext(ctx, "udp4", server)
		},
	}
	net.DefaultResolver = cfResolver

	cfDialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Resolver:  cfResolver,
	}
	http.DefaultTransport = &http.Transport{
		DialContext:           cfDialer.DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// corsMiddleware sets the CORS headers for API clients (see
// core.SetCORSHeaders for why the wildcard origin is safe).
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The gRPC-web bridge sets its own CORS headers and answers its own
		// preflight, on /grpc/ and on the bare /mycelium.<Service>/ paths.
		if strings.HasPrefix(r.URL.Path, "/grpc/") || strings.HasPrefix(r.URL.Path, "/mycelium.") {
			next.ServeHTTP(w, r)
			return
		}

		core.SetCORSHeaders(w, "GET, POST, PUT, DELETE, OPTIONS", "Content-Type, X-Auth-Token", "")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// securityHeadersMiddleware sets baseline browser hardening. The dashboard,
// the web app (/app) and the unauthenticated media endpoints share one
// origin: no MIME sniffing, no framing, no Referer leak of signed URLs, and
// a sandbox CSP on endpoints that relay third-party bytes.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "same-origin")
		p := r.URL.Path
		if p == "/img" || p == "/plugin-icon" || strings.HasPrefix(p, "/plugin-icon/") || strings.HasPrefix(p, "/proxy/") {
			h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		}
		next.ServeHTTP(w, r)
	})
}

// maxRequestBody is the largest request body accepted on path p: a few KB
// of JSON everywhere except plugin upload, the plugin file editor and the
// gRPC-web bridge.
func maxRequestBody(p string) int64 {
	switch {
	case p == "/admin/lua-plugins/upload":
		return 64 << 20
	case strings.HasPrefix(p, "/admin/lua-plugins/file/"):
		return 4 << 20
	case strings.HasPrefix(p, "/grpc/") || strings.HasPrefix(p, "/mycelium."):
		return 8 << 20 // gRPC's own default max message is 4 MiB
	}
	return 1 << 20
}

func bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody(r.URL.Path))
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	applyMemoryLimit()
	log.SetOutput(api.UILogs)

	// Daily log file data/logs/<date>.log (reopened on the first write after
	// midnight).
	logDir := core.AppPath("data", "logs")
	if err := os.MkdirAll(logDir, 0755); err == nil {
		api.UILogs.SetFileOutput(newDailyLogFile(logDir))
	}

	log.Println("[server] avvio...")

	managers.Settings.Load()
	// The dashboard setting wins over MYCELIUM_EGRESS_IPV6 once saved.
	if v := managers.Settings.GetString("egress_ipv6", ""); v != "" {
		core.SetEgressPreferIPv6(core.ParseBoolish(v))
	}

	if err := managers.InitDB(); err != nil {
		log.Fatalf("[server] DB init failed: %v", err)
	}

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	if err := managers.InitRedis(redisAddr); err != nil {
		log.Printf("[server] Redis unavailable (%s): %v — running without cache", redisAddr, err)
	}

	core.SetupHTTPClient()

	// Optional browser service: an external process implementing a small HTTP
	// API (/v1/navigate, /v1/eval, /v1/sniff, /v1/fetch, /health — see
	// internal/managers/browser_client.go) used by mycelium.browser.* and, for
	// upstream fetches, by the HLS proxy. Not configured: those SDK calls fail
	// closed and upstream fetches go out directly.
	if browserURL := strings.TrimSpace(os.Getenv("MYCELIUM_BROWSER_URL")); browserURL != "" {
		if err := managers.ConnectBrowserClient(browserURL, os.Getenv("MYCELIUM_BROWSER_API_KEY")); err != nil {
			log.Printf("[server] browser service client setup failed: %v", err)
		}
	} else {
		log.Printf("[server] servizio browser non configurato (MYCELIUM_BROWSER_URL): mycelium.browser.* non disponibile, fetch upstream diretti")
	}

	if strings.TrimSpace(os.Getenv("MYCELIUM_STATIC_PAIRING_CODE")) != "" {
		if pileus.StaticPairingCode() == "" {
			log.Printf("[server] MYCELIUM_STATIC_PAIRING_CODE ignorato: servono almeno 10 caratteri")
		} else {
			log.Printf("[server] codice di abbinamento statico attivo: qualunque dispositivo che lo conosce può abbinarsi")
		}
	}

	// Plugin egress proxy: VPN_PROXY_URL (full URL, e.g. socks5://host:1080),
	// WARP_SOCKS5_ADDR (bare host:port), else the value last saved from the
	// dashboard.
	vpnProxy := os.Getenv("VPN_PROXY_URL")
	if vpnProxy == "" {
		vpnProxy = os.Getenv("WARP_SOCKS5_ADDR")
	}
	if vpnProxy == "" {
		vpnProxy = managers.Settings.GetString("vpn_proxy_url", "")
	}
	if vpnProxy != "" {
		engine.LuaPlugins.SetProxyAddr(vpnProxy)
	}
	// Seed the "warp" egress profile from that proxy; plugins pick exits by name.
	managers.InitEgressDefaults(vpnProxy)
	// Restart the wireproxy subprocesses of the saved WireGuard profiles (they
	// die with the process). In the background: not needed to serve.
	core.SafeGo("managers/reapply-wireproxy-boot", managers.ReapplyWireproxyAtBoot)
	if err := engine.LuaPlugins.LoadAll(core.AppPath("plugins")); err != nil {
		log.Printf("[server] lua plugin scan error: %v", err)
	}
	// Master secret for the device JWTs, the proxy-URL signature and the admin
	// session cookie.
	jwtSecret := loadOrCreateJWTSecret()
	// Signs every /proxy/* URL mycelium mints, so the proxy isn't an open relay.
	core.SetProxySignKey(jwtSecret)
	// Signs the admin session cookie (self-verifying: survives restarts).
	api.SetAdminSessionKey(jwtSecret)
	// Device JWT key: derived, like the two keys above.
	pileusJWTKey := pileus.DeriveJWTSecret(jwtSecret)
	grpcAddr := managers.Settings.GetString("pileus_grpc_port", "50051")
	// gRPC TLS is on by default: clients pin the certificate fingerprint
	// published by /pileus/info. pileus_grpc_tls=false serves plaintext.
	var tlsCert *tls.Certificate
	if managers.Settings.GetBool("pileus_grpc_tls", true) {
		cert, err := pileus.GenerateOrLoadTLSCert(managers.Settings.GetString, managers.Settings.SaveInternal)
		if err != nil {
			log.Printf("[server] generazione certificato TLS gRPC fallita, si prosegue in chiaro: %v", err)
		} else {
			tlsCert = &cert
		}
	}
	pileusSrv := pileus.Start(":"+grpcAddr, pileusJWTKey, tlsCert)

	// Offline downloads: best-effort, a failure only disables them.
	downloads.SetSignKey(jwtSecret)
	if dm, err := downloads.Init(core.AppPath("data", "downloads"), pileus.DownloadResolver); err != nil {
		log.Printf("[server] download offline disattivati: %v", err)
	} else {
		downloads.M = dm
		dm.Start(context.Background())
		if !dm.FFmpegAvailable() {
			log.Printf("[server] ffmpeg non trovato: i download offline restano disattivati")
		}
	}

	core.StartAppEngine(core.LauncherConfig{
		IsSetupDone:       managers.Settings.IsSetupDone(),
		UpdateDomainsFunc: managers.Domain.UpdateActiveDomains,
		GetSettingFunc:    managers.Settings.GetString,
	})

	// LAN discovery responder (internal/api/discovery.go).
	api.StartDiscovery()

	mux := http.NewServeMux()

	fs := http.FileServer(http.Dir(core.AppPath("web", "static")))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fs.ServeHTTP(w, r)
	})))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		activePlugins := make([]string, 0)
		for _, mf := range engine.LuaPlugins.GetMeta() {
			activePlugins = append(activePlugins, mf.ID)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":           "online",
			"active_providers": activePlugins,
			"message":          fmt.Sprintf("Mycelium ready — %d plugin loaded.", len(activePlugins)),
		})
	})

	api.SetupRoutes(mux)
	api.InternalRoutes(mux)
	api.AdminRoutes(mux)

	// gRPC-web endpoint for the web app: /grpc/<pkg.Service>/<Method>.
	grpcWeb := pileus.GRPCWebHandler()
	mux.Handle("POST /grpc/", http.StripPrefix("/grpc", grpcWeb))
	mux.Handle("OPTIONS /grpc/", http.StripPrefix("/grpc", grpcWeb))
	// Prefix-less fallback: package:grpc web clients resolve the absolute
	// method path against the channel URI, dropping "/grpc". Mount the bridge
	// on each service's subtree (keep in sync with pileus.Start).
	for _, svc := range []string{
		"/mycelium.AuthService/",
		"/mycelium.MediaPipeline/",
		"/mycelium.PluginService/",
	} {
		mux.Handle("POST "+svc, grpcWeb)
		mux.Handle("OPTIONS "+svc, grpcWeb)
	}

	port := managers.Settings.GetString("server_port", "8000")
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           securityHeadersMiddleware(corsMiddleware(api.SameOriginMiddleware(bodyLimitMiddleware(mux)))),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("[server] listen :%s: %v", port, err)
	}
	log.Printf("[server] listening on :%s", port)

	go func() {
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[server] fatal: %v", err)
		}
	}()

	log.Println("🏁 Mycelium pronto.")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("[server] shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv.SetKeepAlivesEnabled(false)
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[server] shutdown error: %v", err)
	}

	// GracefulStop sends HTTP/2 GOAWAY instead of cutting connections; bounded
	// so a long-lived RPC can't hang shutdown.
	stopped := make(chan struct{})
	go func() {
		pileusSrv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		log.Println("[pileus] graceful stop timed out after 5s, forcing")
		pileusSrv.Stop()
	}

	core.StopScheduler()
	managers.CloseBrowserClient()
	engine.LuaPlugins.Shutdown()

	log.Println("[server] shutdown complete.")
}

// loadOrCreateJWTSecret returns the 32-byte master secret stored in
// settings, generating it on first boot.
func loadOrCreateJWTSecret() []byte {
	const key = "pileus_jwt_secret"
	stored := managers.Settings.GetString(key, "")
	if stored != "" {
		b, err := hex.DecodeString(stored)
		if err == nil && len(b) == 32 {
			return b
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("[server] jwt secret gen: %v", err)
	}
	_ = managers.Settings.SaveInternal(map[string]any{key: hex.EncodeToString(b)})
	return b
}
