package managers

import (
	"database/sql"
	"log"
	"mycelium/internal/core"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

var dbFile = core.AppPath("data", "mycelium.db")

type DBManager struct {
	db *sql.DB
}

var DB *DBManager

// InitDB apre la connessione e crea le tabelle.
func InitDB() error {
	os.MkdirAll(core.AppPath("data"), 0755)

	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return err
	}

	// Imposta configurazioni SQLite
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		log.Printf("⚠️ PRAGMA journal_mode=WAL non applicato: %v", err)
	}
	if _, err := db.Exec("PRAGMA synchronous=NORMAL;"); err != nil {
		log.Printf("⚠️ PRAGMA synchronous=NORMAL non applicato: %v", err)
	}
	// Senza busy_timeout, SQLite fallisce SUBITO con "database is locked"
	// quando due connessioni del pool scrivono nello stesso istante (es.
	// dashboard che salva un'impostazione mentre un task Lua scrive
	// watch_history) invece di attendere che il writer in corso finisca — WAL
	// permette lettori concorrenti ma resta un solo writer alla volta. 5s
	// copre ampiamente le scritture di questo processo (nessuna transazione
	// è mai così lunga).
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		log.Printf("⚠️ PRAGMA busy_timeout non applicato: %v", err)
	}

	// Creazione Tabelle
	schema := `
	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT
	);
	CREATE TABLE IF NOT EXISTS watch_history (
		client_id TEXT,
		provider_id TEXT,
		playable_id TEXT,
		title TEXT,
		poster TEXT,
		progress_time REAL,
		total_time REAL,
		is_completed BOOLEAN,
		last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (client_id, provider_id, playable_id)
	);
	CREATE TABLE IF NOT EXISTS clients (
		client_id TEXT PRIMARY KEY,
		secret_key TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS pileus_devices (
		device_id   TEXT PRIMARY KEY,
		created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		last_seen_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS pileus_profiles (
		profile_id  TEXT PRIMARY KEY,
		device_id   TEXT NOT NULL REFERENCES pileus_devices(device_id) ON DELETE CASCADE,
		name        TEXT NOT NULL,
		avatar_url  TEXT NOT NULL DEFAULT '',
		is_child    BOOLEAN NOT NULL DEFAULT 0,
		pin_hash    TEXT,   -- dead column: parental PIN feature removed 2026-09-06, no code reads it
		created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	// Migrations (idempotent — SQLite errors on duplicate column are silently ignored).
	_, _ = db.Exec(`ALTER TABLE watch_history ADD COLUMN parent_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE watch_history ADD COLUMN navigation_context TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE pileus_devices ADD COLUMN label TEXT NOT NULL DEFAULT ''`)
	// revoked_at: NULL = active. Set to kill every existing JWT for a device
	// without deleting its profiles (FK ON DELETE CASCADE) — the auth
	// interceptor rejects a token whose device row is missing or revoked.
	// Cleared again on a successful re-pairing (AuthorizeDevice).
	_, _ = db.Exec(`ALTER TABLE pileus_devices ADD COLUMN revoked_at TIMESTAMP`)
	// rating/genres/plot/year: continue-watching card metadata beyond title/poster.
	// genres is comma-joined (SQLite has no array type), same convention the
	// Flutter client already uses when displaying a joined genre list.
	_, _ = db.Exec(`ALTER TABLE watch_history ADD COLUMN rating REAL NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE watch_history ADD COLUMN genres TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE watch_history ADD COLUMN plot TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE watch_history ADD COLUMN year INTEGER NOT NULL DEFAULT 0`)
	// Per-profile client preferences (subtitle style, audio/sub language, …).
	// Opaque JSON blob — the Pileus client owns the schema; the server only
	// stores and echoes it. Profiles are server-wide, so this follows the
	// person across every device paired to the server.
	_, _ = db.Exec(`ALTER TABLE pileus_profiles ADD COLUMN preferences TEXT NOT NULL DEFAULT '{}'`)

	log.Println("🗄️ Database inizializzato (mycelium.db).")
	DB = &DBManager{db: db}
	return nil
}

// AddClient salva un nuovo dispositivo [cite: 2]
func (m *DBManager) AddClient(clientID, secretKey string) error {
	query := "INSERT INTO clients (client_id, secret_key) VALUES (?, ?)"
	_, err := m.db.Exec(query, clientID, secretKey)
	return err
}

// GetClients recupera la lista per la dashboard [cite: 2]
func (m *DBManager) GetClients() ([]map[string]string, error) {
	rows, err := m.db.Query("SELECT client_id FROM clients")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			results = append(results, map[string]string{"client_id": id})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// GetAllClientSecrets returns every clientID → secretKey pair.
// Used to validate Stremio addon tokens without an extra DB column.
func (m *DBManager) GetAllClientSecrets() (map[string]string, error) {
	rows, err := m.db.Query("SELECT client_id, secret_key FROM clients")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, secret string
		if err := rows.Scan(&id, &secret); err == nil {
			out[id] = secret
		}
	}
	return out, rows.Err()
}

// GetClientSecret recupera la chiave segreta per generare il token HMAC
func (m *DBManager) GetClientSecret(clientID string) (string, error) {
	var secret string
	err := m.db.QueryRow("SELECT secret_key FROM clients WHERE client_id = ?", clientID).Scan(&secret)
	return secret, err
}

// RemoveClient elimina fisicamente il dispositivo dal database
func (m *DBManager) RemoveClient(clientID string) error {
	_, err := m.db.Exec("DELETE FROM clients WHERE client_id = ?", clientID)
	return err
}

// GetSetting recupera un'impostazione
func (m *DBManager) GetSetting(key string) (string, error) {
	var value string
	err := m.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil // Nessun risultato, non è un vero errore fatale
	}
	return value, err
}

// SetSetting effettua l'Upsert di un'impostazione
func (m *DBManager) SetSetting(key, value string) error {
	query := `
		INSERT INTO settings (key, value) 
		VALUES (?, ?) 
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`
	_, err := m.db.Exec(query, key, value)
	return err
}

// UpsertProgress aggiorna la cronologia.
// Se parentID non è vuoto, rimuove la entry precedente della stessa serie prima di inserire.
func (m *DBManager) UpsertProgress(clientID, providerID, playableID, parentID, navigationContext, title, poster string, currentTime, totalTime float64, rating float64, genres []string, plot string, year int32) error {
	if parentID != "" {
		_, _ = m.db.Exec(
			`DELETE FROM watch_history WHERE client_id=? AND provider_id=? AND parent_id=? AND playable_id!=?`,
			clientID, providerID, parentID, playableID,
		)
	}

	isCompleted := 0
	if totalTime > 0 && (currentTime/totalTime) > 0.90 {
		isCompleted = 1
	}
	genresJoined := strings.Join(genres, ",")

	query := `
		INSERT INTO watch_history
		(client_id, provider_id, playable_id, parent_id, navigation_context, title, poster, progress_time, total_time, is_completed, rating, genres, plot, year, last_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(client_id, provider_id, playable_id) DO UPDATE SET
			parent_id          = excluded.parent_id,
			-- keep-if-empty, same as title/poster below: a position-only
			-- heartbeat (or a play path that doesn't know the series name)
			-- must not blank an already-stored navigation_context.
			navigation_context = CASE WHEN excluded.navigation_context != '' THEN excluded.navigation_context ELSE navigation_context END,
			title              = CASE WHEN excluded.title  != '' THEN excluded.title  ELSE title  END,
			poster             = CASE WHEN excluded.poster != '' THEN excluded.poster ELSE poster END,
			progress_time      = excluded.progress_time,
			total_time         = CASE WHEN excluded.total_time > 0 THEN excluded.total_time ELSE total_time END,
			is_completed       = excluded.is_completed,
			rating             = CASE WHEN excluded.rating > 0 THEN excluded.rating ELSE rating END,
			genres             = CASE WHEN excluded.genres != '' THEN excluded.genres ELSE genres END,
			plot               = CASE WHEN excluded.plot   != '' THEN excluded.plot   ELSE plot   END,
			year               = CASE WHEN excluded.year   > 0   THEN excluded.year   ELSE year   END,
			last_updated       = CURRENT_TIMESTAMP
	`
	_, err := m.db.Exec(query, clientID, providerID, playableID, parentID, navigationContext, title, poster, currentTime, totalTime, isCompleted, rating, genresJoined, plot, year)
	return err
}

type WatchHistoryEntry struct {
	ProviderID        string   `json:"provider_id"`
	PlayableID        string   `json:"playable_id"`
	ParentId          string   `json:"parent_id,omitempty"`
	NavigationContext string   `json:"navigation_context,omitempty"`
	Title             string   `json:"title"`
	Poster            string   `json:"poster"`
	ProgressTime      float64  `json:"progress_time"`
	TotalTime         float64  `json:"total_time"`
	LastUpdated       string   `json:"last_updated"`
	Rating            float64  `json:"rating"`
	Genres            []string `json:"genres,omitempty"`
	Plot              string   `json:"plot,omitempty"`
	Year              int32    `json:"year,omitempty"`
}

// DeleteProgress removes a single watch_history entry for a client.
func (m *DBManager) DeleteProgress(clientID, providerID, playableID string) error {
	_, err := m.db.Exec(
		"DELETE FROM watch_history WHERE client_id = ? AND provider_id = ? AND playable_id = ?",
		clientID, providerID, playableID,
	)
	return err
}

// GetContinueWatching returns in-progress (non-completed) items for a client,
// most recently watched first, capped at limit.
// If parentID is non-empty, filters to items with that parent_id.
func (m *DBManager) GetContinueWatching(clientID string, limit int, parentID ...string) ([]WatchHistoryEntry, error) {
	filter := ""
	args := []any{clientID}
	if len(parentID) > 0 && parentID[0] != "" {
		filter = " AND parent_id = ?"
		args = append(args, parentID[0])
	}
	args = append(args, limit)

	rows, err := m.db.Query(`
		SELECT provider_id, playable_id, parent_id, navigation_context, title, poster, progress_time, total_time, last_updated, rating, genres, plot, year
		FROM watch_history
		WHERE client_id = ? AND is_completed = 0
			AND progress_time >= 30
			AND (total_time <= 0 OR ((progress_time / total_time) >= 0.03 AND (progress_time / total_time) < 0.95))
		`+filter+`
		ORDER BY last_updated DESC
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []WatchHistoryEntry
	for rows.Next() {
		var e WatchHistoryEntry
		var genresJoined string
		if err := rows.Scan(&e.ProviderID, &e.PlayableID, &e.ParentId, &e.NavigationContext, &e.Title, &e.Poster, &e.ProgressTime, &e.TotalTime, &e.LastUpdated, &e.Rating, &genresJoined, &e.Plot, &e.Year); err != nil {
			return nil, err
		}
		if genresJoined != "" {
			e.Genres = strings.Split(genresJoined, ",")
		}
		results = append(results, e)
	}
	return results, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// Raw SQL passthrough — used by internal packages (e.g. pileus handlers)
// that need direct DB access without adding high-level methods here.
// ─────────────────────────────────────────────────────────────────────────────

func (m *DBManager) Exec(query string, args ...any) (sql.Result, error) {
	return m.db.Exec(query, args...)
}

func (m *DBManager) Query(query string, args ...any) (*sql.Rows, error) {
	return m.db.Query(query, args...)
}

func (m *DBManager) QueryRow(query string, args ...any) *sql.Row {
	return m.db.QueryRow(query, args...)
}
