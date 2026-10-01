package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// serverStartTime tracks when the server process started, for uptime display.
var serverStartTime = time.Now()

// semverLikeRe matches the "vX.Y.Z" / "X.Y.Z" shape of release tags.
var semverLikeRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

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
	if v, ok := payload["egress_ipv6"]; ok {
		// Applies to the next dial.
		core.SetEgressPreferIPv6(core.ParseBoolish(fmt.Sprint(v)))
	}
	if _, ok := payload["http_profile"]; ok {
		// Rebuild the proxy's upstream clients so the change applies at once.
		resetUpstreamClients()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func getCoreResources(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(managers.GetCoreStats())
}

// getPluginsInfo returns every plugin's settings schema and lifecycle state
// for the dashboard.
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
			// discarded_states: Lua states discarded after an entrypoint timeout; a
			// growing number flags a stuck task or entrypoint.
			"discarded_states": engine.LuaPlugins.DiscardedStates(mf.ID),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// getEnricherBindings returns an empty result: the dashboard still calls it.
func getEnricherBindings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"bindings": map[string]any{}, "providers": []any{}})
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
	// Persisted so it survives a restart (main.go reads it back when
	// VPN_PROXY_URL/WARP_SOCKS5_ADDR are unset).
	if err := managers.Settings.Save(map[string]any{"vpn_proxy_url": payload.Addr}); err != nil {
		log.Printf("[vpn] impossibile salvare lo stato del proxy: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":   payload.Addr != "",
		"addr": payload.Addr,
	})
}

// clearCacheHandler invalidates the in-memory cache.
func clearCacheHandler(w http.ResponseWriter, r *http.Request) {
	managers.Cache.Flush()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// effectiveServerHost returns the configured server host: the setting, else
// MYCELIUM_SERVER_HOST.
func effectiveServerHost() string {
	if h := managers.Settings.GetString("server_host", ""); h != "" {
		return h
	}
	return os.Getenv("MYCELIUM_SERVER_HOST")
}

// getAdminInfo returns version, uptime and the dashboard's settings view.
func getAdminInfo(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(serverStartTime)
	latest := core.LatestVersion()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"uptime_seconds": int64(uptime.Seconds()),
		"version":        core.Version,
		"latest_version": latest,
		// Computed here so the dashboard shows "update available" only when latest
		// is really newer than core.Version.
		"update_available": core.IsNewerVersion(latest, core.Version),
		"start_time":       serverStartTime.Format(time.RFC3339),
		// Address clients use to reach the server, for the proxy URLs ("" =
		// autodetect).
		"server_host":     effectiveServerHost(),
		"server_host_env": os.Getenv("MYCELIUM_SERVER_HOST") != "",
		"in_docker":       os.Getenv("MYCELIUM_DOCKER") == "1",
		// "owner/repo" of the Pileus web build (POST /admin/pileus-web/update).
		"pileus_web_repo": managers.Settings.GetString("pileus_web_repo", "Lotho33/pileus"),
		// Prefer IPv6 on direct connections (core.egressPreferIPv6).
		"egress_ipv6": core.EgressPreferIPv6(),
		// Version of the installed Pileus web build ("" = none).
		"pileus_web_version": func() string {
			if info, ok := pileusWebVersion(); ok {
				return info.Version
			}
			return ""
		}(),
		"pileus_web_build": func() string {
			if info, ok := pileusWebVersion(); ok {
				return info.BuildNumber
			}
			return ""
		}(),
		// Upstream path of the HLS proxy: "browser-relay" when a browser service is
		// configured, unless MYCELIUM_HTTP_PROFILE=standard.
		"http_profile": func() string {
			if strings.EqualFold(os.Getenv("MYCELIUM_HTTP_PROFILE"), "standard") {
				return "standard"
			}
			return "browser-relay"
		}(),
		// Offline downloads: effective values with defaults.
		"download_enabled":               managers.Settings.GetString("download_enabled", "1") != "0",
		"download_quota_gb":              settingNumber("download_quota_gb", 20),
		"download_min_free_gb":           settingNumber("download_min_free_gb", 3),
		"download_retention_days":        settingNumber("download_retention_days", 7),
		"download_max_mbps":              settingNumber("download_max_mbps", 0),
		"download_pace":                  settingNumber("download_pace", 4),
		"download_pause_while_streaming": managers.Settings.GetString("download_pause_while_streaming", "1") != "0",
		"download_delete_after_fetch":    managers.Settings.GetString("download_delete_after_fetch", "0") == "1",
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
	// Validate the tag before any other use: a malformed GitHub answer must be
	// rejected, not taken as "up to date".
	if !semverLikeRe.MatchString(latest) {
		log.Printf("[core] update rifiutato: formato versione non valido da GitHub: %q", latest)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"detail": "formato versione non valido, aggiornamento annullato"})
		return
	}

	// Up to date also when the running build is ahead of the latest release.
	if !core.IsNewerVersion(latest, core.Version) {
		json.NewEncoder(w).Encode(map[string]string{"status": "up_to_date", "version": core.Version})
		return
	}

	// MYCELIUM_DOCKER is set by the image.
	if os.Getenv("MYCELIUM_DOCKER") == "1" {
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "manual_required",
			"detail":  "Ambiente Docker: aggiorna con `docker pull` e riavvia il container.",
			"version": latest,
		})
		return
	}

	// Only Docker deployments are supported: nothing to update automatically.
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "manual_required",
		"detail":  "mycelium è supportato solo in Docker: aggiorna l'immagine (docker compose pull && docker compose up -d).",
		"version": latest,
		"url":     "https://github.com/Lotho33/mycelium-core/releases/latest",
	})
}

// restartService restarts the process gracefully, for settings read only at
// boot: it sends itself SIGTERM (the normal shutdown path) and the
// container's restart policy brings it back. Outside Docker nothing
// restarts it, and the response says so.
func restartService(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	inDocker := os.Getenv("MYCELIUM_DOCKER") == "1"
	json.NewEncoder(w).Encode(map[string]any{
		"status": "restarting",
		"docker": inDocker,
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	log.Println("[admin] riavvio del servizio richiesto dalla dashboard")
	go func() {
		// Let the response reach the browser before shutting down.
		time.Sleep(300 * time.Millisecond)
		proc, err := os.FindProcess(os.Getpid())
		if err != nil {
			log.Printf("[admin] riavvio: os.FindProcess fallito: %v", err)
			return
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			log.Printf("[admin] riavvio: invio SIGTERM fallito: %v", err)
		}
	}()
}

// settingNumber reads a numeric setting, def when unset or invalid.
func settingNumber(key string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(managers.Settings.GetString(key, "")), 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}
