// Admin-side profile management ("Utenti" tab): list/create/rename/delete
// the server-wide profiles a Pileus client otherwise only sees through its
// own gRPC calls. Same auth boundary as every other /admin/* route.
package api

import (
	"encoding/json"
	"net/http"

	"mycelium/internal/pileus"
)

// RegisterPileusProfileRoutes wires the profile-management endpoints onto mux.
func RegisterPileusProfileRoutes(mux *http.ServeMux) {
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/pileus/profiles", auth(listPileusProfiles))
	mux.HandleFunc("POST /admin/pileus/profiles", auth(createPileusProfile))
	mux.HandleFunc("POST /admin/pileus/profiles/{id}", auth(updatePileusProfile))
	mux.HandleFunc("DELETE /admin/pileus/profiles/{id}", auth(deletePileusProfile))
}

// GET /admin/pileus/profiles → {profiles: [{profile_id,name,avatar_url,created_at}]}
func listPileusProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := pileus.ListProfilesAdmin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": profiles})
}

type profileRequestBody struct {
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// POST /admin/pileus/profiles  body: {"name":"...", "avatar_url":"..."}
func createPileusProfile(w http.ResponseWriter, r *http.Request) {
	var body profileRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid JSON body"})
		return
	}
	if body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "name required"})
		return
	}
	p, err := pileus.CreateProfileAdmin(body.Name, body.AvatarURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// POST /admin/pileus/profiles/{id}  body: {"name":"...", "avatar_url":"..."}
func updatePileusProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "profile id required"})
		return
	}
	var body profileRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid JSON body"})
		return
	}
	if body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "name required"})
		return
	}
	if err := pileus.UpdateProfileAdmin(id, body.Name, body.AvatarURL); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"profile_id": id, "name": body.Name, "avatar_url": body.AvatarURL,
	})
}

// DELETE /admin/pileus/profiles/{id}
func deletePileusProfile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "profile id required"})
		return
	}
	if err := pileus.DeleteProfileAdmin(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
