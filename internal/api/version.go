package api

import (
	"encoding/json"
	"net/http"

	"mycelium/internal/core"
)

func getVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"server_version":     core.Version,
		"min_client_version": "1.0.0",
		"latest_version":     core.LatestVersion(),
	})
}
