// Egress management (Fase A "gestione VPN più semplice"): the operator keeps a
// small registry of network exits — "direct" (built-in), "warp" (the microwarp
// sidecar, seeded from VPN_PROXY_URL), plus any socks5/http proxy they add —
// and each plugin picks which one its video flow takes. Nothing here forces ALL
// traffic through a VPN: a plugin left on "direct" is untouched.
//
// Storage + resolution live in internal/managers/egress.go; this file is just
// the admin HTTP surface the dashboard's "Uscite di rete" card talks to.
package api

import (
	"encoding/json"
	"io"
	"net/http"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// RegisterEgressRoutes wires the egress registry endpoints onto mux.
func RegisterEgressRoutes(mux *http.ServeMux) {
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/egress", auth(listEgress))
	mux.HandleFunc("POST /admin/egress", auth(upsertEgress))
	mux.HandleFunc("POST /admin/egress/wireproxy", auth(upsertWireproxyEgress))
	mux.HandleFunc("POST /admin/egress/{name}/enabled", auth(setEgressEnabled))
	mux.HandleFunc("POST /admin/egress/{name}/test", auth(testEgress))
	mux.HandleFunc("DELETE /admin/egress/{name}", auth(deleteEgress))
	mux.HandleFunc("POST /admin/lua-plugins/egress/{plugin_id}", auth(setPluginEgress))
}

// POST /admin/egress/wireproxy — multipart: `name` + `conf` (a WireGuard .conf).
// Saves the config, (re)registers the single wireproxy-backed egress and brings
// the sidecar up.
func upsertWireproxyEgress(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(256 << 10); err != nil {
		http.Error(w, "form non valido", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	if name == "" {
		name = "wg"
	}
	file, _, err := r.FormFile("conf")
	if err != nil {
		http.Error(w, "file .conf mancante", http.StatusBadRequest)
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 128<<10))
	if err != nil {
		http.Error(w, "lettura file fallita", http.StatusBadRequest)
		return
	}
	if err := managers.UpsertWireproxyEgress(name, string(raw)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profiles": managers.EgressProfiles()})
}

// GET /admin/egress → {profiles:[{name,kind,proxy_url,enabled,port,running}]}
// `running` is filled in only for wireproxy kinds (a live Docker check).
func listEgress(w http.ResponseWriter, r *http.Request) {
	profiles := managers.EgressProfiles()
	out := make([]map[string]any, 0, len(profiles))
	for _, p := range profiles {
		row := map[string]any{
			"name": p.Name, "kind": p.Kind, "proxy_url": p.ProxyURL, "enabled": p.Enabled,
		}
		if p.Kind == "wireproxy" {
			row["port"] = p.Port
			row["running"] = managers.WireproxyRunning(p.Name)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

// POST /admin/egress {name,kind,proxy_url} → add or replace a profile.
func upsertEgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		ProxyURL string `json:"proxy_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "JSON non valido", http.StatusBadRequest)
		return
	}
	if err := managers.UpsertEgressProfile(managers.EgressProfile{
		Name:     body.Name,
		Kind:     body.Kind,
		ProxyURL: body.ProxyURL,
		Enabled:  true,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profiles": managers.EgressProfiles()})
}

// POST /admin/egress/{name}/enabled {enabled:bool}
func setEgressEnabled(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "JSON non valido", http.StatusBadRequest)
		return
	}
	if err := managers.SetEgressEnabled(r.PathValue("name"), body.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profiles": managers.EgressProfiles()})
}

// DELETE /admin/egress/{name}
func deleteEgress(w http.ResponseWriter, r *http.Request) {
	if err := managers.DeleteEgressProfile(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profiles": managers.EgressProfiles()})
}

// POST /admin/egress/{name}/test → runs a connectivity trace
// (1.1.1.1/cdn-cgi/trace) through the profile's proxy and reports the exit IP
// (same check as /admin/vpn/test, but for one named profile instead of the
// legacy single proxy).
func testEgress(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	proxyURL, ok := managers.ResolveEgressProxy(name)
	if name == managers.EgressDirect {
		proxyURL = "" // trace goes out direct
	}
	if !ok {
		http.Error(w, "uscita disabilitata", http.StatusBadRequest)
		return
	}
	if proxyURL == "" && name != managers.EgressDirect {
		http.Error(w, "uscita senza proxy configurato", http.StatusBadRequest)
		return
	}

	var directIP string
	if direct, err := traceIP(http.DefaultClient); err == nil {
		directIP = direct["ip"]
	}

	client := http.DefaultClient
	if proxyURL != "" {
		client = engine.NewScrapingClient(proxyURL)
	}
	trace, err := traceIP(client)
	if err != nil {
		http.Error(w, "la richiesta attraverso "+name+" è fallita: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"exit_ip":   trace["ip"],
		"exit_loc":  trace["loc"],
		"warp":      trace["warp"],
		"direct_ip": directIP,
		"leaking":   proxyURL != "" && directIP != "" && directIP == trace["ip"],
	})
}

// POST /admin/lua-plugins/egress/{plugin_id} {egress:"direct"|"warp"|…}
// Persists lua:{id}:global:egress — the per-plugin egress selection PluginEgress
// reads. An empty/"direct" value clears it back to the manifest default.
func setPluginEgress(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	if !engine.LuaPlugins.Has(pluginID) {
		http.Error(w, "plugin non trovato", http.StatusNotFound)
		return
	}
	var body struct {
		Egress string `json:"egress"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "JSON non valido", http.StatusBadRequest)
		return
	}
	// Validate against the registry so a typo can't silently disable a plugin.
	valid := body.Egress == "" || body.Egress == managers.EgressDirect
	for _, p := range managers.EgressProfiles() {
		if p.Name == body.Egress {
			valid = true
			break
		}
	}
	if !valid {
		http.Error(w, "uscita sconosciuta: "+body.Egress, http.StatusBadRequest)
		return
	}
	key := "lua:" + pluginID + ":global:" + engine.EgressSettingID
	if err := managers.Settings.Save(map[string]any{key: body.Egress}); err != nil {
		http.Error(w, "salvataggio fallito: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "egress": engine.LuaPlugins.PluginEgress(pluginID)})
}
