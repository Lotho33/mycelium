package core

import (
	"errors"
	"strings"

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

	// The hash is made from the password exactly as typed (GetPasswordHash
	// never trimmed), so compare that first. The trimmed form is kept as a
	// fallback for a stray space picked up while typing a password that was
	// set without one — trimming alone locked out anyone whose password
	// really starts or ends with a space.
	if bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(adminKey)) == nil {
		return nil
	}
	if trimmed := strings.TrimSpace(adminKey); trimmed != adminKey &&
		bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(trimmed)) == nil {
		return nil
	}
	return errors.New("password errata")
}
