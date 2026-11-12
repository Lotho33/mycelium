package api

import (
	"encoding/json"
	"log"
	"net/http"

	"mycelium/internal/backup"
	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// Restore from a backup during the first-run setup (/setup → "Restore from
// a backup"): the same staging, summary and per-item choice as the
// dashboard's import, without having to create an admin account first.
//
// Same gate as saveSetup: open only while no admin password exists, only
// from the local network and under a LAN Host name. The admin password is
// written right away (the backup's, or a new one typed on the page), so the
// setup is closed before the restart that applies the rest.

func registerSetupRestoreRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /setup/restore/inspect", setupRestoreInspect)
	mux.HandleFunc("POST /setup/restore/apply", setupRestoreApply)
	mux.HandleFunc("DELETE /setup/restore", setupRestoreDiscard)
}

// setupRestoreDiscard drops a backup read on the setup page and then
// abandoned ("start from scratch").
func setupRestoreDiscard(w http.ResponseWriter, r *http.Request) {
	setupMu.Lock()
	defer setupMu.Unlock()
	if !setupOpen(w, r) {
		return
	}
	if err := backup.Discard(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setupOpen answers the request itself and returns false when the setup is
// closed or the caller isn't allowed to run it.
func setupOpen(w http.ResponseWriter, r *http.Request) bool {
	if managers.Settings.IsSetupDone() {
		http.Error(w, "Setup already completed", http.StatusForbidden)
		return false
	}
	if !setupAllowedFrom(realIP(r)) {
		log.Printf("[setup] rifiutato ripristino da %s (non in rete locale)", realIP(r))
		http.Error(w, "The initial setup is only allowed from the local network", http.StatusForbidden)
		return false
	}
	if !setupHostAllowed(r.Host) {
		http.Error(w, "Open the setup using the server's IP address or its local network name", http.StatusForbidden)
		return false
	}
	return true
}

func setupRestoreInspect(w http.ResponseWriter, r *http.Request) {
	if !setupOpen(w, r) {
		return
	}
	inspectBackupHandler(w, r)
}

func setupRestoreApply(w http.ResponseWriter, r *http.Request) {
	setupMu.Lock()
	defer setupMu.Unlock()
	if !setupOpen(w, r) {
		return
	}
	var body struct {
		Selection       backup.Selection `json:"selection"`
		Password        string           `json:"password"`
		PasswordConfirm string           `json:"password_confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// The admin password: the backup's when picked, otherwise a new one.
	hash := ""
	fromBackup := body.Selection.Has(backup.SectionIdentity, backup.ItemAdminPassword)
	if fromBackup {
		hash = backup.StagedAdminHash()
		if hash == "" {
			http.Error(w, "The backup holds no admin password: choose a new one", http.StatusBadRequest)
			return
		}
	} else {
		if len(body.Password) < 8 {
			http.Error(w, "The password must be at least 8 characters long", http.StatusBadRequest)
			return
		}
		if body.Password != body.PasswordConfirm {
			http.Error(w, "The two passwords do not match", http.StatusBadRequest)
			return
		}
		h, err := core.GetPasswordHash(body.Password)
		if err != nil {
			http.Error(w, "Could not hash the password", http.StatusInternalServerError)
			return
		}
		hash = h
	}

	// Validates the choice against the staged backup before anything is
	// written: a bad request leaves the setup open.
	safety, err := backup.Schedule(body.Selection)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := managers.Settings.SaveInternal(map[string]any{"master_admin_hash": hash}); err != nil {
		_ = backup.Discard()
		http.Error(w, "Could not save the configuration", http.StatusInternalServerError)
		return
	}
	// Signed with the current key: still valid after the restart unless the
	// backup's server keys were imported too (then the dashboard asks to sign
	// in, with the restored password).
	setAdminSessionCookie(w, r)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"password_from_backup": fromBackup,
		"server_keys":          body.Selection.Has(backup.SectionIdentity, "server_keys"),
		"safety_copy":          safety,
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	log.Printf("[setup] ripristino da backup programmato al primo avvio (password dal backup: %v): riavvio", fromBackup)
	setupRestart()
}

// setupRestart restarts the process to apply the restore (a var for tests).
var setupRestart = selfRestartSoon
