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

// traceIP interroga l'endpoint di trace di Cloudflare attraverso client e
// restituisce l'IP pubblico e il paese (loc) con cui la richiesta è uscita.
// È l'unico modo affidabile per sapere se il tunnel funziona davvero: il
// sidecar WARP può risultare "up" pur non instradando nulla (registrazione
// fallita, healthcheck non ancora passato...).
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

// testVPNConnection godoc
//
//	@Summary		Testa la connessione VPN
//	@Description	Confronta l'IP pubblico visto direttamente con quello visto attraverso il proxy configurato, per verificare che il tunnel instrada davvero il traffico.
//	@Tags			Admin
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/vpn/test [post]
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

	vpn, err := traceIP(engine.NewScrapingClient(addr))
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
		// warp riporta il campo "warp" del trace Cloudflare (on/plus/off): a
		// differenza di vpn_ip/direct_ip (che dimostrano solo che il traffico
		// passa da *un* proxy), questo conferma che è specificamente il
		// sidecar WARP a instradarlo — utile perché un proxy generico
		// (VPN_PROXY_URL può puntare a qualsiasi SOCKS5/HTTP) darebbe comunque
		// "leaking=false" senza essere WARP.
		"warp": vpn["warp"],
		// leaking=true è l'allarme: l'IP visto "attraverso" il proxy è lo
		// stesso di quello diretto, quindi il tunnel non sta instradando nulla.
		"leaking": directIP != "" && directIP == vpn["ip"],
	})
}
