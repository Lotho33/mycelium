package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// serverStartTime tracks when the server process started, for uptime display.
var serverStartTime = time.Now()

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
