// Admin-side endpoints for Pileus device pairing (internal/pileus/pairing.go
// holds the actual code state and the gRPC-side verification). Generating a
// code requires an authenticated admin session — same boundary as every
// other /admin/* route — so only someone already logged into the dashboard
// can mint one; Pileus itself never calls anything here, it only ever
// presents the code to AuthService.AuthorizeDevice over gRPC.
package api

import (
	"net/http"

	"mycelium/internal/pileus"
)

// RegisterPairingRoutes wires the pairing endpoints onto mux.
func RegisterPairingRoutes(mux *http.ServeMux) {
	auth := adminAuthMiddleware
	mux.HandleFunc("POST /admin/pileus/pairing/token", auth(generatePairingToken))
	mux.HandleFunc("GET /admin/pileus/pairing/token", auth(pairingTokenStatus))
}

// POST /admin/pileus/pairing/token → {code, expires_at} — mints a new code,
// replacing any still-pending one (only one active at a time).
func generatePairingToken(w http.ResponseWriter, r *http.Request) {
	code, expiresAt := pileus.GeneratePairingCode()
	writeJSON(w, http.StatusOK, map[string]any{
		"code":       code,
		"expires_at": expiresAt.Unix(),
	})
}

// GET /admin/pileus/pairing/token → {active, expires_at, used, device_id} —
// lets the dashboard poll for "a device just paired" without re-displaying
// the code itself.
func pairingTokenStatus(w http.ResponseWriter, r *http.Request) {
	active, expiresAt, used, deviceID := pileus.PairingStatus()
	out := map[string]any{"active": active, "used": used}
	if active {
		out["expires_at"] = expiresAt.Unix()
	}
	if used {
		out["device_id"] = deviceID
	}
	writeJSON(w, http.StatusOK, out)
}
