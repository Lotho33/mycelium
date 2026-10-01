package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

type contextKey int

const clientIDKey contextKey = 0

func clientAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Auth-Token")
		if token == "" {
			http.Error(w, `{"detail": "Non autorizzato"}`, http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(token, ":", 3)
		if len(parts) != 3 {
			http.Error(w, `{"detail": "Token non valido"}`, http.StatusUnauthorized)
			return
		}

		secret, err := managers.DB.GetClientSecret(parts[0])
		if err != nil {
			http.Error(w, `{"detail": "Client non riconosciuto"}`, http.StatusUnauthorized)
			return
		}

		clientID, err := core.VerifyDynamicToken(token, secret)
		if err != nil {
			http.Error(w, `{"detail": "Token non valido o scaduto"}`, http.StatusUnauthorized)
			return
		}

		ctx := r.Context()
		ctx = context.WithValue(ctx, clientIDKey, clientID)
		next(w, r.WithContext(ctx))
	}
}

func getVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"server_version":     core.Version,
		"min_client_version": "1.0.0",
		"latest_version":     core.LatestVersion(),
	})
}

func deleteProgress(w http.ResponseWriter, r *http.Request) {
	clientID, _ := r.Context().Value(clientIDKey).(string)
	providerID := r.PathValue("provider_id")
	playableID := r.PathValue("playable_id")

	if err := managers.DB.DeleteProgress(clientID, providerID, playableID); err != nil {
		log.Printf("❌ [progress/delete] client=%s err=%v", clientID, err)
		http.Error(w, `{"detail": "Errore rimozione progress"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func postProgress(w http.ResponseWriter, r *http.Request) {
	clientID, _ := r.Context().Value(clientIDKey).(string)

	var body struct {
		ProviderID        string   `json:"provider_id"`
		PlayableID        string   `json:"playable_id"`
		ParentID          string   `json:"parent_id"`
		NavigationContext string   `json:"navigation_context"`
		Title             string   `json:"title"`
		Poster            string   `json:"poster"`
		ProgressTime      float64  `json:"progress_time"`
		TotalTime         float64  `json:"total_time"`
		Rating            float64  `json:"rating"`
		Genres            []string `json:"genres"`
		Plot              string   `json:"plot"`
		Year              int32    `json:"year"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"detail": "Body non valido"}`, http.StatusBadRequest)
		return
	}
	if body.ProviderID == "" || body.PlayableID == "" {
		http.Error(w, `{"detail": "provider_id e playable_id obbligatori"}`, http.StatusBadRequest)
		return
	}

	// This API carries no season/episode: 0 means unknown.
	if err := managers.DB.UpsertProgress(clientID, body.ProviderID, body.PlayableID, body.ParentID, body.NavigationContext, body.Title, body.Poster, body.ProgressTime, body.TotalTime, body.Rating, body.Genres, body.Plot, body.Year, 0, 0); err != nil {
		log.Printf("❌ [progress] client=%s err=%v", clientID, err)
		http.Error(w, `{"detail": "Errore salvataggio progress"}`, http.StatusInternalServerError)
		return
	}

	fetchWatchHistoryLogo(clientID, body.ProviderID, body.ParentID, body.PlayableID)

	w.WriteHeader(http.StatusNoContent)
}

// fetchWatchHistoryLogo fetches, in the background, the title logo of a
// continue-watching entry that has none, through the plugin's get_details
// (logo_url). Best-effort; NeedsLogo makes it happen once per title, not per
// heartbeat.
func fetchWatchHistoryLogo(clientID, providerID, parentID, playableID string) {
	if !engine.LuaPlugins.Has(providerID) || !engine.LuaPlugins.IsOperational(providerID) {
		return // not loaded, or stopped: a stopped plugin must not run Lua
	}
	target := managers.WatchHistoryLogoTarget{
		ClientID: clientID, ProviderID: providerID, ParentID: parentID, PlayableID: playableID,
	}
	needs, err := managers.DB.NeedsLogo(target)
	if err != nil || !needs {
		return
	}
	mediaID := parentID
	if mediaID == "" {
		mediaID = playableID
	}
	core.SafeGo("api/watch-history-logo", func() {
		raw, err := engine.LuaPlugins.CallEntrypointJSON(providerID, "get_details", map[string]any{"media_id": mediaID}, "")
		if err != nil {
			return
		}
		var details struct {
			LogoURL string `json:"logo_url"`
		}
		if err := json.Unmarshal(raw, &details); err != nil || details.LogoURL == "" {
			return
		}
		if err := managers.DB.UpdateWatchHistoryLogo(target, details.LogoURL); err != nil {
			log.Printf("⚠️ [progress] logo update failed client=%s provider=%s: %v", clientID, providerID, err)
		}
	})
}

func getContinueWatching(w http.ResponseWriter, r *http.Request) {
	clientID, _ := r.Context().Value(clientIDKey).(string)

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20
	}

	parentID := r.URL.Query().Get("parent_id")
	entries, err := managers.DB.GetContinueWatching(clientID, limit, parentID)
	if err != nil {
		log.Printf("❌ [continue_watching] client=%s err=%v", clientID, err)
		http.Error(w, `{"detail": "Errore recupero cronologia"}`, http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []managers.WatchHistoryEntry{}
	}

	if pid := r.URL.Query().Get("provider_id"); pid != "" {
		filtered := entries[:0]
		for _, e := range entries {
			if e.ProviderID == pid {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"items": entries})
}
