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
	"syscall"
	"time"

	"mycelium/internal/api"
	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
	"mycelium/internal/pileus"
)

// defaultMemLimitBytes is the soft heap cap applied when the operator hasn't
// set GOMEMLIMIT explicitly. Sized for the low-RAM hosts this service targets
// (whole docker-compose stack + OS under ~1 GiB): it leaves headroom in the
// container's hard memory limit for the WebKit subprocess (~60-80 MB, outside
// Go's GC control) and other non-heap overhead.
const defaultMemLimitBytes = 350 << 20 // 350 MiB

// applyMemoryLimit caps the Go heap so the GC works harder as usage approaches
// the limit instead of letting RSS balloon (e.g. during a burst of concurrent
// catalog syncs) before the next collection catches up. It's a soft limit —
// Go never refuses an allocation because of it, it just collects more eagerly,
// so this cannot itself cause an OOM; it only reduces how high RSS spikes.
// Respects a operator-set GOMEMLIMIT env var (parsed automatically by the Go
// runtime) instead of overriding it.
func applyMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	debug.SetMemoryLimit(defaultMemLimitBytes)
}

// init runs before main() and forces ALL DNS lookups in the process to use
// Cloudflare 1.1.1.1 over IPv4 UDP, bypassing the ISP/router IPv6 resolver.
// This affects net.DefaultResolver (used by http.DefaultTransport and any
// transport without an explicit DialContext), and also replaces
// http.DefaultTransport so third-party libraries that use it get Cloudflare too.
func init() {
	cfResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			conn, err := d.DialContext(ctx, "udp4", "1.1.1.1:53")
			if err != nil {
				conn, err = d.DialContext(ctx, "udp4", "8.8.8.8:53")
			}
			return conn, err
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

// corsMiddleware allows cross-origin requests from hub clients (Kodi addons, mobile apps).
// Wildcard origin is intentional: hub clients are native apps, not browsers, so there is
// no cookie-based session to steal. Admin routes use SameSite=Strict cookies and are
// never called cross-origin by legitimate clients.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The gRPC-web bridge sets its own (grpc-web-specific) CORS headers and
		// answers its own preflight — don't clobber it here. This covers both
		// the canonical /grpc/ mount and the bare method path fallback
		// (/mycelium.<Service>/<Method>) some grpc-web clients hit directly.
		if strings.HasPrefix(r.URL.Path, "/grpc/") || strings.HasPrefix(r.URL.Path, "/mycelium.") {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Auth-Token")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func main() {
	applyMemoryLimit()
	log.SetOutput(api.UILogs)

	// File di log giornaliero in data/logs/<data>.log — dailyLogFile riapre da
	// sé il file del giorno nuovo alla prima scrittura dopo mezzanotte, così
	// il processo (che sta su per giorni) ruota davvero e non scrive per
	// sempre nel file del giorno di boot.
	logDir := core.AppPath("data", "logs")
	if err := os.MkdirAll(logDir, 0755); err == nil {
		api.UILogs.SetFileOutput(newDailyLogFile(logDir))
	}

	log.Println("[server] avvio...")

	managers.Settings.Load()

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

	// The browser/extractor engine is cobweb (../cobweb, a standalone Rust
	// sidecar) — a separate service (its own container in docker-compose.yml)
	// — mycelium no longer launches a browser itself. Lua plugins reach it
	// exclusively through this client (see engine.currentBrowserClient), over
	// plain HTTP — see internal/managers/browser_client.go for why not gRPC.
	cobwebAddr := os.Getenv("COBWEB_ADDR")
	if cobwebAddr == "" {
		cobwebAddr = "http://localhost:8191"
	}
	if err := managers.ConnectBrowserClient(cobwebAddr); err != nil {
		log.Printf("[server] browser service client setup failed: %v", err)
	}

	// Load Lua plugins (plugins/{id}/manifest.yaml + init.lua).
	// VPN_PROXY_URL is a full proxy URL (e.g. socks5://warp:1080 or http://host:8888);
	// WARP_SOCKS5_ADDR is the legacy bare-host form, kept for back-compat.
	// Se nessuna delle due è impostata, ripristina l'ultimo indirizzo scelto
	// dall'admin UI (persistito in data/config.json da setVPNProxy) — così lo
	// stato on/off della VPN sopravvive a un riavvio invece di tornare sempre
	// disabilitato.
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
	// Egress registry (Fase A gestione VPN): semina il profilo "warp" dal
	// vecchio VPN_PROXY_URL / vpn_proxy_url così i deployment esistenti
	// mantengono un'uscita già configurata. I plugin scelgono per nome.
	managers.InitEgressDefaults(vpnProxy)
	// Fase C: riporta su i sidecar wireproxy-<nome> salvati (un riavvio non
	// deve lasciarli morti). In goroutine: al primo avvio può dover fare un
	// `docker pull` dell'immagine wireproxy, che non deve bloccare il boot
	// del server HTTP/gRPC.
	core.SafeGo("managers/reapply-wireproxy-boot", managers.ReapplyWireproxyAtBoot)
	if err := engine.LuaPlugins.LoadAll(core.AppPath("plugins")); err != nil {
		log.Printf("[server] lua plugin scan error: %v", err)
	}
	// Pileus gRPC server — load or generate a stable JWT secret.
	jwtSecret := loadOrCreateJWTSecret()
	// Same master secret keys the HMAC on every /proxy/* URL we mint, so the
	// HLS proxy can't be driven as an open relay to an arbitrary URL.
	core.SetProxySignKey(jwtSecret)
	grpcAddr := managers.Settings.GetString("pileus_grpc_port", "50051")
	// TLS di default (2026-08-19): il client Pileus ora pinna il certificato
	// via il fingerprint esposto da /pileus/info (vedi internal/pileus.
	// TLSFingerprint, internal/api/setup.go pileusInfo), quindi non c'è più
	// bisogno di aspettare — resta comunque un settings override esplicito
	// per chi deve tornare temporaneamente al comportamento in chiaro
	// legacy (client più vecchi, debug di rete).
	var tlsCert *tls.Certificate
	if managers.Settings.GetString("pileus_grpc_tls", "true") == "true" {
		cert, err := pileus.GenerateOrLoadTLSCert(managers.Settings.GetString, managers.Settings.Save)
		if err != nil {
			log.Printf("[server] generazione certificato TLS gRPC fallita, si prosegue in chiaro: %v", err)
		} else {
			tlsCert = &cert
		}
	}
	pileusSrv := pileus.Start(":"+grpcAddr, jwtSecret, tlsCert)

	core.StartAppEngine(core.LauncherConfig{
		IsSetupDone:       managers.Settings.IsSetupDone(),
		UpdateDomainsFunc: managers.Domain.UpdateActiveDomains,
		GetSettingFunc:    managers.Settings.GetString,
	})

	// Risponditore UDP broadcast per la scoperta LAN — vedi
	// internal/api/discovery.go. Sostituisce il vecchio mDNS via Avahi (mai
	// realmente funzionante nel deployment Docker: nessun avahi-daemon
	// nell'immagine).
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

	// gRPC-web endpoint — used by the Pileus Flutter web app (PWA).
	//
	// Canonical mount: /grpc/<pkg.Service>/<Method>.
	grpcWeb := pileus.GRPCWebHandler()
	mux.Handle("POST /grpc/", http.StripPrefix("/grpc", grpcWeb))
	mux.Handle("OPTIONS /grpc/", http.StripPrefix("/grpc", grpcWeb))
	// Prefix-less fallback: grpc-web clients built with package:grpc resolve the
	// generated method path — absolute, "/mycelium.<Service>/<Method>" — against
	// the channel URI with Uri.resolve() semantics, which replaces the whole
	// base path and so drops the "/grpc" segment (a trailing slash on the base
	// does NOT help — the method path is absolute). The request then arrives
	// here without the prefix. Mount the bridge on each registered service's
	// subtree so those land on it, without a blunt catch-all on "/". Keep this
	// list in sync with pileus.Start's RegisterXxxServer calls.
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
		Handler:           corsMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("[server] listen :%s: %v", port, err)
	}
	log.Printf("[server] listening on :%s", port)
	log.Println("🏁 Mycelium pronto.")

	go func() {
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[server] fatal: %v", err)
		}
	}()

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

	// GracefulStop sends HTTP/2 GOAWAY to connected clients (the Pileus frontend
	// apps) instead of letting process exit sever their connections with a bare
	// TCP RST. Bounded: a stuck long-lived RPC (e.g. ResolveStream) must not hang
	// shutdown forever — force-stop after 5s same as any other client would see
	// on a hard restart, just not silently on every clean one.
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

// loadOrCreateJWTSecret returns a stable 32-byte random secret stored in settings.
// Generated once on first boot; persists across restarts.
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
	_ = managers.Settings.Save(map[string]any{key: hex.EncodeToString(b)})
	return b
}
