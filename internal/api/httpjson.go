package api

import (
	"encoding/json"
	"net/http"
)

// writeJSON is the shared JSON-response helper for /admin/* handlers across
// this package.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
