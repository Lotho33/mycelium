package api

import (
	"encoding/json"
	"net/http"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// changeAdminPasswordHandler changes the admin password. Body:
// {"current_password": "…", "new_password": "…"} (min. 8 characters).
// 400: bad body or password too short; 401: wrong current password.
func changeAdminPasswordHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "body non valido", http.StatusBadRequest)
		return
	}

	// A session cookie alone isn't enough to set a new password: the current
	// one is required, as for the destructive actions.
	if err := core.VerifyAdmin(body.CurrentPassword, managers.Settings.MasterAdminHash()); err != nil {
		http.Error(w, "password attuale errata", http.StatusUnauthorized)
		return
	}

	// Same minimum as the setup password.
	if len(body.NewPassword) < 8 {
		http.Error(w, "la nuova password deve essere di almeno 8 caratteri", http.StatusBadRequest)
		return
	}

	hashed, err := core.GetPasswordHash(body.NewPassword)
	if err != nil {
		http.Error(w, "errore generazione hash", http.StatusInternalServerError)
		return
	}

	// SaveInternal: master_admin_hash is not writable through Save.
	if err := managers.Settings.SaveInternal(map[string]any{"master_admin_hash": hashed}); err != nil {
		http.Error(w, "errore salvataggio", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
