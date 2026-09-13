package api

import (
	"encoding/json"
	"net/http"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// changeAdminPasswordHandler godoc
//
//	@Summary		Cambia la password admin
//	@Description	Richiede la password admin attuale (verificata via bcrypt) e la nuova password (min. 8 caratteri). L'hash viene scritto direttamente, bypassando l'allowlist generica di /admin/settings/save — master_admin_hash non è scrivibile da lì (vedi settings.go).
//	@Tags			Admin
//	@Accept			json
//	@Param			body	body	object{current_password=string,new_password=string}	true	"password attuale e nuova password"
//	@Produce		json
//	@Success		200	{object}	map[string]string
//	@Failure		400	{string}	string	"nuova password troppo corta o body non valido"
//	@Failure		401	{string}	string	"password attuale errata"
//	@Router			/admin/password/change [post]
func changeAdminPasswordHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "body non valido", http.StatusBadRequest)
		return
	}

	// La sessione admin (adminAuthMiddleware) prova solo che il chiamante ha un
	// cookie valido — un cookie rubato via XSS/dispositivo condiviso basterebbe
	// altrimenti a impiantare una password a scelta. Come per le azioni
	// distruttive in admin_wipe.go, si richiede di nuovo la password attuale.
	if err := core.VerifyAdmin(body.CurrentPassword, managers.Settings.MasterAdminHash()); err != nil {
		http.Error(w, "password attuale errata", http.StatusUnauthorized)
		return
	}

	// Stesso vincolo minimo della password di setup (setup.go: saveSetup).
	if len(body.NewPassword) < 8 {
		http.Error(w, "la nuova password deve essere di almeno 8 caratteri", http.StatusBadRequest)
		return
	}

	hashed, err := core.GetPasswordHash(body.NewPassword)
	if err != nil {
		http.Error(w, "errore generazione hash", http.StatusInternalServerError)
		return
	}

	// SaveInternal, non Save: scrittura diretta di master_admin_hash,
	// bypassando isAllowedKey — solo questo endpoint (password attuale già
	// verificata sopra) e il setup one-shot possono farlo.
	if err := managers.Settings.SaveInternal(map[string]any{"master_admin_hash": hashed}); err != nil {
		http.Error(w, "errore salvataggio", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
