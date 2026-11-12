package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"mycelium/internal/backup"
	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// Backup and restore of the whole configuration (internal/backup).
//
//	POST /admin/backup/export          {password, passphrase?} → the .mycelium file
//	POST /admin/backup/inspect         multipart: passphrase?, then file → Summary
//	GET  /admin/backup/status          staged backup, schedule, last result
//	POST /admin/backup/apply           {password, selection} → schedule + restart
//	POST /admin/backup/discard         drop the staged backup
//	GET  /admin/backup/safety/{name}   download a pre-import safety copy
func RegisterBackupRoutes(mux *http.ServeMux) {
	auth := adminAuthMiddleware
	// Export hands out every secret and apply replaces them: both re-check the
	// admin password, rate limited like the other destructive actions.
	mux.HandleFunc("POST /admin/backup/export", rateLimitMiddleware(wipeLimiter, auth(exportBackupHandler)))
	mux.HandleFunc("POST /admin/backup/apply", rateLimitMiddleware(wipeLimiter, auth(applyBackupHandler)))
	mux.HandleFunc("POST /admin/backup/inspect", auth(inspectBackupHandler))
	mux.HandleFunc("GET /admin/backup/status", auth(backupStatusHandler))
	mux.HandleFunc("POST /admin/backup/discard", auth(discardBackupHandler))
	mux.HandleFunc("GET /admin/backup/safety/{name}", auth(safetyCopyHandler))
}

// MaxBackupUpload caps an uploaded backup.
const MaxBackupUpload = 2 << 30

// checkAdminPassword answers 422 (not 401/403: the dashboard reads those as
// "session gone") when password isn't the admin's.
func checkAdminPassword(w http.ResponseWriter, password string) bool {
	if password == "" {
		http.Error(w, "password richiesta", http.StatusBadRequest)
		return false
	}
	if err := core.VerifyAdmin(password, managers.Settings.MasterAdminHash()); err != nil {
		http.Error(w, "password errata", http.StatusUnprocessableEntity)
		return false
	}
	return true
}

func exportBackupHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password   string `json:"password"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "richiesta non valida", http.StatusBadRequest)
		return
	}
	if !checkAdminPassword(w, body.Password) {
		return
	}
	// Built to a file first: the download starts only once the archive is
	// complete, and an error can still be answered as one.
	tmp, err := os.CreateTemp("", "mycelium-export-*")
	if err != nil {
		http.Error(w, "errore file temporaneo", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := backup.Export(tmp, body.Passphrase); err != nil {
		log.Printf("[backup] export fallito: %v", err)
		http.Error(w, "backup non riuscito: "+err.Error(), http.StatusInternalServerError)
		return
	}
	st, _ := tmp.Stat()
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "backup non riuscito", http.StatusInternalServerError)
		return
	}
	name := backup.FileName(time.Now())
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if st != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	log.Printf("[backup] esportata la configurazione (%s, cifrato=%v)", name, body.Passphrase != "")
	io.Copy(w, tmp)
}

// inspectBackupHandler streams the upload straight into backup.Stage. The
// passphrase field must come before the file (the dashboard sends it so).
func inspectBackupHandler(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "upload non valido", http.StatusBadRequest)
		return
	}
	passphrase := ""
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			http.Error(w, "nessun file ricevuto", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "upload non valido", http.StatusBadRequest)
			return
		}
		switch part.FormName() {
		case "passphrase":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			passphrase = string(b)
		case "file":
			sum, err := backup.Stage(part, passphrase)
			if err != nil {
				status := http.StatusBadRequest
				var mbe *http.MaxBytesError
				switch {
				case errors.Is(err, backup.ErrNeedPassphrase), errors.Is(err, backup.ErrWrongPassphrase):
					status = http.StatusUnprocessableEntity
				case errors.As(err, &mbe):
					status = http.StatusRequestEntityTooLarge
				}
				http.Error(w, err.Error(), status)
				return
			}
			writeJSON(w, http.StatusOK, sum)
			return
		}
	}
}

func backupStatusHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, backup.CurrentStatus())
}

func discardBackupHandler(w http.ResponseWriter, r *http.Request) {
	if err := backup.Discard(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func applyBackupHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password  string           `json:"password"`
		Selection backup.Selection `json:"selection"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "richiesta non valida", http.StatusBadRequest)
		return
	}
	if !checkAdminPassword(w, body.Password) {
		return
	}
	safety, err := backup.Schedule(body.Selection)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	inDocker := os.Getenv("MYCELIUM_DOCKER") == "1"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "safety_copy": safety, "docker": inDocker})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	log.Printf("[backup] ripristino programmato (copia di sicurezza %s): riavvio", safety)
	selfRestartSoon()
}

func safetyCopyHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p := backup.SafetyCopyPath(name)
	if p == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, p)
}
