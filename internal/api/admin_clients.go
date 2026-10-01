package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"mycelium/internal/managers"
)

// Hub API clients: each has a client_id and a secret key signing the
// short-lived X-Auth-Token of every hub request (core.VerifyDynamicToken).

func generateSecretKey() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand non disponibile: " + err.Error())
	}
	return base64.URLEncoding.EncodeToString(b)
}

// listClients lists the hub clients.
func listClients(w http.ResponseWriter, r *http.Request) {
	secrets, err := managers.DB.GetAllClientSecrets()
	if err != nil {
		http.Error(w, "Errore caricamento client", 500)
		return
	}
	type clientEntry struct {
		ClientID string `json:"client_id"`
	}
	result := make([]clientEntry, 0, len(secrets))
	for id := range secrets {
		result = append(result, clientEntry{ClientID: id})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// addClient creates a client with a random secret key (query: client_id).
func addClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		http.Error(w, "ID mancante", 400)
		return
	}

	newSecret := generateSecretKey()

	err := managers.DB.AddClient(clientID, newSecret)
	if err != nil {
		http.Error(w, "Errore salvataggio: ID già esistente?", 400)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"client_id":  clientID,
		"SECRET_KEY": newSecret,
	})
}

// removeClient deletes a client.
func removeClient(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("client_id")

	w.Header().Set("Content-Type", "application/json")
	if err := managers.DB.RemoveClient(clientID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"detail": "Errore DB"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}

// generateDevToken returns an X-Auth-Token valid 5 minutes, for testing the
// API (query: client_id).
func generateDevToken(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	secret, err := managers.DB.GetClientSecret(clientID)
	if err != nil {
		http.Error(w, "Client non trovato", http.StatusNotFound)
		return
	}

	timestamp := time.Now().Unix()
	message := fmt.Sprintf("%s:%d", clientID, timestamp)

	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(message))
	signature := hex.EncodeToString(h.Sum(nil))

	token := fmt.Sprintf("%s:%d:%s", clientID, timestamp, signature)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"X-Auth-Token": token})
}
