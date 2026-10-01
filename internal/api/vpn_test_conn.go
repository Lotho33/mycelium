package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mycelium/internal/engine"
)

// traceIP queries Cloudflare's trace endpoint through client and returns
// the public IP and country the request left from: the reliable way to know
// whether a tunnel really routes traffic.
func traceIP(client *http.Client) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://1.1.1.1/cdn-cgi/trace", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if out["ip"] == "" {
		return nil, fmt.Errorf("risposta inattesa dal servizio di trace")
	}
	return out, nil
}

// testVPNConnection compares the public IP seen directly with the one seen
// through the configured proxy.
func testVPNConnection(w http.ResponseWriter, r *http.Request) {
	addr := engine.LuaPlugins.GetProxyAddr()
	if addr == "" {
		http.Error(w, "routing VPN non attivo — imposta e salva l'indirizzo del proxy prima di testare", http.StatusBadRequest)
		return
	}

	var directIP, directLoc string
	if direct, err := traceIP(http.DefaultClient); err == nil {
		directIP, directLoc = direct["ip"], direct["loc"]
	}

	vpn, err := traceIP(engine.NewPluginHTTPClient(addr))
	if err != nil {
		http.Error(w, "la richiesta attraverso "+addr+" è fallita: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"vpn_ip":     vpn["ip"],
		"vpn_loc":    vpn["loc"],
		"direct_ip":  directIP,
		"direct_loc": directLoc,
		// warp is the trace's "warp" field (on/plus/off): it confirms the traffic
		// goes through WARP specifically, not just some proxy.
		"warp": vpn["warp"],
		// leaking=true: the address through the proxy equals the direct one, the
		// tunnel routes nothing.
		"leaking": directIP != "" && directIP == vpn["ip"],
	})
}
