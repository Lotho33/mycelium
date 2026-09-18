package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// GetPasswordHash genera l'hash bcrypt
func GetPasswordHash(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

// VerifyAdmin verifica la password dell'amministratore
func VerifyAdmin(adminKey, storedHash string) error {
	if storedHash == "" {
		return errors.New("setup non completato")
	}

	storedHash = strings.TrimSpace(strings.ReplaceAll(storedHash, "\r", ""))

	if !strings.HasPrefix(storedHash, "$2") {
		return errors.New("hash admin non in formato bcrypt valido")
	}

	err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(strings.TrimSpace(adminKey)))
	if err != nil {
		return errors.New("password errata")
	}
	return nil
}

// VerifyDynamicToken decodifica il token, verifica la scadenza e la firma HMAC
func VerifyDynamicToken(token, clientSecret string) (string, error) {
	if token == "" {
		return "", errors.New("X-Auth-Token mancante")
	}

	parts := strings.SplitN(token, ":", 3)
	if len(parts) != 3 {
		return "", errors.New("formato token non valido")
	}

	clientID := parts[0]
	timestampStr := parts[1]
	signature := parts[2]

	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return "", errors.New("timestamp non valido")
	}

	// Controllo scadenza (300 secondi)
	if time.Now().Unix()-timestamp > 300 || time.Now().Unix()-timestamp < -300 {
		return "", errors.New("token scaduto")
	}

	if clientSecret == "" {
		log.Printf("Tentativo di accesso con client ID sconosciuto: %s", clientID)
		return "", errors.New("client non riconosciuto")
	}

	// Creazione della firma HMAC
	mac := hmac.New(sha256.New, []byte(clientSecret))
	mac.Write([]byte(fmt.Sprintf("%s:%d", clientID, timestamp)))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(signature)) != 1 {
		log.Printf("Firma HMAC non valida per client: %s", clientID)
		return "", errors.New("firma token non valida")
	}

	return clientID, nil
}
