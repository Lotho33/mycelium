package managers

import (
	"database/sql"
	"fmt"
	"log"
	"mycelium/internal/core"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var dbFile = core.AppPath("data", "mycelium.db")

type DBManager struct {
	db *sql.DB
}

var DB *DBManager

// NewDBManager wraps an already-open *sql.DB (tests build their own).
func NewDBManager(db *sql.DB) *DBManager {
	return &DBManager{db: db}
}

// openSQLite opens path with the connection settings in the DSN: busy_timeout
// and synchronous are per-connection, and modernc.org/sqlite applies each
// _pragma to every new connection of the pool.
func openSQLite(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" + // wait for the single WAL writer instead of failing
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	return sql.Open("sqlite", dsn)
}

// InitDB opens the database and creates the tables.
func InitDB() error {
	os.MkdirAll(core.AppPath("data"), 0755)

	db, err := openSQLite(dbFile)
	if err != nil {
		return err
	}

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
		is_child    BOOLEAN NOT NULL DEFAULT 0, -- dead column: no child-profile concept, no code reads it (removed 2026-09-16)
		pin_hash    TEXT,   -- dead column: parental PIN feature removed 2026-09-06, no code reads it
		created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	-- Per-profile plugin secrets (mycelium.context.get/set_secret). They used
	-- to live only in Redis, which runs as an evicting LRU cache: a login a
	-- user typed once could silently disappear.
	CREATE TABLE IF NOT EXISTS plugin_secrets (
		plugin_id  TEXT NOT NULL,
		profile_id TEXT NOT NULL,
		key        TEXT NOT NULL,
		value      TEXT NOT NULL,
		PRIMARY KEY (plugin_id, profile_id, key)
	);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	// Migrations are idempotent: SQLite's "duplicate column" error from an
	// already-applied ALTER is ignored, anything else is logged.
	runMigration(db, `ALTER TABLE watch_history ADD COLUMN parent_id TEXT NOT NULL DEFAULT ''`)
	runMigration(db, `ALTER TABLE watch_history ADD COLUMN navigation_context TEXT NOT NULL DEFAULT ''`)
	runMigration(db, `ALTER TABLE pileus_devices ADD COLUMN label TEXT NOT NULL DEFAULT ''`)
	// revoked_at: NULL = active. Set, it invalidates every JWT of the device;
	// the auth interceptor rejects a missing or revoked device.
	runMigration(db, `ALTER TABLE pileus_devices ADD COLUMN revoked_at TIMESTAMP`)
	// rating/genres/plot/year: continue-watching card metadata; genres is
	// comma-joined.
	runMigration(db, `ALTER TABLE watch_history ADD COLUMN rating REAL NOT NULL DEFAULT 0`)
	runMigration(db, `ALTER TABLE watch_history ADD COLUMN genres TEXT NOT NULL DEFAULT ''`)
	runMigration(db, `ALTER TABLE watch_history ADD COLUMN plot TEXT NOT NULL DEFAULT ''`)
	runMigration(db, `ALTER TABLE watch_history ADD COLUMN year INTEGER NOT NULL DEFAULT 0`)
	// Per-profile client preferences: an opaque JSON blob owned by the client.
	runMigration(db, `ALTER TABLE pileus_profiles ADD COLUMN preferences TEXT NOT NULL DEFAULT '{}'`)
	// logo_url: the title's logo, fetched in the background from the plugin's
	// get_details.
	runMigration(db, `ALTER TABLE watch_history ADD COLUMN logo_url TEXT NOT NULL DEFAULT ''`)
	// Profile access PIN (bcrypt, NULL = none) and the devices trusted for a
	// protected profile (internal/pileus/profile_pin.go). The old pin_hash
	// column is unused.
	runMigration(db, `ALTER TABLE pileus_profiles ADD COLUMN access_pin_hash TEXT`)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS pileus_profile_trust (
		device_id  TEXT NOT NULL,
		profile_id TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (device_id, profile_id)
	)`); err != nil {
		return err
	}
	// season_number/episode_number: 0 for non-episodic items.
	runMigration(db, `ALTER TABLE watch_history ADD COLUMN season_number INTEGER NOT NULL DEFAULT 0`)
	runMigration(db, `ALTER TABLE watch_history ADD COLUMN episode_number INTEGER NOT NULL DEFAULT 0`)

	log.Println("🗄️ Database inizializzato (mycelium.db).")
	DB = &DBManager{db: db}
	return nil
}

// isDuplicateColumnError reports whether err is SQLite's "duplicate column
// name" error (an ALTER TABLE ADD COLUMN already applied).
func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column")
}

// runMigration runs an idempotent schema migration: a duplicate-column error
// is expected; other errors are logged without failing InitDB.
func runMigration(db *sql.DB, query string) {
	if _, err := db.Exec(query); err != nil && !isDuplicateColumnError(err) {
		log.Printf("⚠️ migrazione DB fallita (%q): %v", query, err)
	}
}

// AddClient stores a new hub client.
func (m *DBManager) AddClient(clientID, secretKey string) error {
	query := "INSERT INTO clients (client_id, secret_key) VALUES (?, ?)"
	_, err := m.db.Exec(query, clientID, secretKey)
	return err
}

// GetClients lists the hub clients for the dashboard.
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

// GetClientSecret returns the client's HMAC secret.
func (m *DBManager) GetClientSecret(clientID string) (string, error) {
	var secret string
	err := m.db.QueryRow("SELECT secret_key FROM clients WHERE client_id = ?", clientID).Scan(&secret)
	return secret, err
}

// RemoveClient deletes a hub client.
func (m *DBManager) RemoveClient(clientID string) error {
	_, err := m.db.Exec("DELETE FROM clients WHERE client_id = ?", clientID)
	return err
}

// GetSetting reads a setting.
func (m *DBManager) GetSetting(key string) (string, error) {
	var value string
	err := m.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil // no row: not an error
	}
	return value, err
}

// SetSetting upserts a setting.
func (m *DBManager) SetSetting(key, value string) error {
	query := `
		INSERT INTO settings (key, value) 
		VALUES (?, ?) 
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`
	_, err := m.db.Exec(query, key, value)
	return err
}

// UpsertProgress records a watch-history entry. With a parentID, the
// previous entry of the same series is replaced.
func (m *DBManager) UpsertProgress(clientID, providerID, playableID, parentID, navigationContext, title, poster string, currentTime, totalTime float64, rating float64, genres []string, plot string, year int32, seasonNumber, episodeNumber int32) error {
	isCompleted := 0
	if totalTime > 0 && (currentTime/totalTime) > 0.90 {
		isCompleted = 1
	}
	genresJoined := strings.Join(genres, ",")

	query := `
		INSERT INTO watch_history
		(client_id, provider_id, playable_id, parent_id, navigation_context, title, poster, progress_time, total_time, is_completed, rating, genres, plot, year, season_number, episode_number, last_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
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
			season_number      = CASE WHEN excluded.season_number  > 0 THEN excluded.season_number  ELSE season_number  END,
			episode_number     = CASE WHEN excluded.episode_number > 0 THEN excluded.episode_number ELSE episode_number END,
			last_updated       = CURRENT_TIMESTAMP
	`
	// The stale sibling row goes in the same transaction as the upsert, so the
	// series is never momentarily missing.
	if parentID == "" {
		_, err := m.db.Exec(query, clientID, providerID, playableID, parentID, navigationContext, title, poster, currentTime, totalTime, isCompleted, rating, genresJoined, plot, year, seasonNumber, episodeNumber)
		return err
	}

	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op once Commit succeeds

	if _, err := tx.Exec(
		`DELETE FROM watch_history WHERE client_id=? AND provider_id=? AND parent_id=? AND playable_id!=?`,
		clientID, providerID, parentID, playableID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(query, clientID, providerID, playableID, parentID, navigationContext, title, poster, currentTime, totalTime, isCompleted, rating, genresJoined, plot, year, seasonNumber, episodeNumber); err != nil {
		return err
	}
	return tx.Commit()
}

type WatchHistoryEntry struct {
	ProviderID        string   `json:"provider_id"`
	PlayableID        string   `json:"playable_id"`
	ParentId          string   `json:"parent_id,omitempty"`
	NavigationContext string   `json:"navigation_context,omitempty"`
	Title             string   `json:"title"`
	Poster            string   `json:"poster"`
	LogoURL           string   `json:"logo_url,omitempty"`
	ProgressTime      float64  `json:"progress_time"`
	TotalTime         float64  `json:"total_time"`
	LastUpdated       string   `json:"last_updated"`
	Rating            float64  `json:"rating"`
	Genres            []string `json:"genres,omitempty"`
	Plot              string   `json:"plot,omitempty"`
	Year              int32    `json:"year,omitempty"`
	SeasonNumber      int32    `json:"season_number,omitempty"`
	EpisodeNumber     int32    `json:"episode_number,omitempty"`
}

// WatchHistoryLogoTarget selects the rows NeedsLogo/UpdateWatchHistoryLogo
// act on: every row of this client+provider with parentID (playableID when
// there is no parent).
type WatchHistoryLogoTarget struct {
	ClientID   string
	ProviderID string
	ParentID   string
	PlayableID string
}

// NeedsLogo reports whether the title's rows still lack a logo_url, so a
// title triggers one fetch, not one per heartbeat.
func (m *DBManager) NeedsLogo(t WatchHistoryLogoTarget) (bool, error) {
	var logo string
	var err error
	if t.ParentID != "" {
		err = m.db.QueryRow(
			`SELECT logo_url FROM watch_history WHERE client_id=? AND provider_id=? AND parent_id=? LIMIT 1`,
			t.ClientID, t.ProviderID, t.ParentID,
		).Scan(&logo)
	} else {
		err = m.db.QueryRow(
			`SELECT logo_url FROM watch_history WHERE client_id=? AND provider_id=? AND playable_id=? LIMIT 1`,
			t.ClientID, t.ProviderID, t.PlayableID,
		).Scan(&logo)
	}
	if err != nil {
		return false, err
	}
	return logo == "", nil
}

// UpdateWatchHistoryLogo sets logo_url on the matching rows that lack one.
func (m *DBManager) UpdateWatchHistoryLogo(t WatchHistoryLogoTarget, logoURL string) error {
	if t.ParentID != "" {
		_, err := m.db.Exec(
			`UPDATE watch_history SET logo_url=? WHERE client_id=? AND provider_id=? AND parent_id=? AND logo_url=''`,
			logoURL, t.ClientID, t.ProviderID, t.ParentID,
		)
		return err
	}
	_, err := m.db.Exec(
		`UPDATE watch_history SET logo_url=? WHERE client_id=? AND provider_id=? AND playable_id=? AND logo_url=''`,
		logoURL, t.ClientID, t.ProviderID, t.PlayableID,
	)
	return err
}

// UpdateProgressPosition refines an existing row's position from
// server-observed activity (segment fetches, StreamSession.RecordFetch). A
// plain UPDATE, never an INSERT: the stream id may differ from the id the
// client reports progress with, and a row without metadata must never be
// created. It is only a fallback for clients that don't report their own
// position: the estimate ignores seeks and read-ahead, so it never
// overwrites a row the client updated within serverEstimateAfter.
func (m *DBManager) UpdateProgressPosition(clientID, providerID, playableID string, currentTime, totalTime float64) error {
	isCompleted := 0
	if totalTime > 0 && (currentTime/totalTime) > 0.90 {
		isCompleted = 1
	}
	_, err := m.db.Exec(`
		UPDATE watch_history SET
			progress_time = ?,
			total_time    = CASE WHEN ? > 0 THEN ? ELSE total_time END,
			is_completed  = ?,
			last_updated  = CURRENT_TIMESTAMP
		WHERE client_id = ? AND provider_id = ? AND playable_id = ?
		  AND last_updated <= datetime('now', ?)`,
		currentTime, totalTime, totalTime, isCompleted,
		clientID, providerID, playableID,
		fmt.Sprintf("-%d seconds", int(serverEstimateAfter.Seconds())),
	)
	return err
}

// serverEstimateAfter: how long the client must have been silent before the
// server's estimate may replace its position.
const serverEstimateAfter = 60 * time.Second

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
	pid := ""
	if len(parentID) > 0 {
		pid = parentID[0]
	}
	return m.GetContinueWatchingFor(clientID, limit, pid, "")
}

// GetContinueWatchingFor is GetContinueWatching with optional parent and
// provider filters, applied before LIMIT.
func (m *DBManager) GetContinueWatchingFor(clientID string, limit int, parentID, providerID string) ([]WatchHistoryEntry, error) {
	filter := ""
	args := []any{clientID}
	if parentID != "" {
		filter += " AND parent_id = ?"
		args = append(args, parentID)
	}
	if providerID != "" {
		filter += " AND provider_id = ?"
		args = append(args, providerID)
	}
	args = append(args, limit)

	// No minimum position: a title shows up as soon as it's opened; is_completed
	// (set above 90%) drops it once finished.
	rows, err := m.db.Query(`
		SELECT provider_id, playable_id, parent_id, navigation_context, title, poster, logo_url, progress_time, total_time, last_updated, rating, genres, plot, year, season_number, episode_number
		FROM watch_history
		WHERE client_id = ? AND is_completed = 0
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
		if err := rows.Scan(&e.ProviderID, &e.PlayableID, &e.ParentId, &e.NavigationContext, &e.Title, &e.Poster, &e.LogoURL, &e.ProgressTime, &e.TotalTime, &e.LastUpdated, &e.Rating, &genresJoined, &e.Plot, &e.Year, &e.SeasonNumber, &e.EpisodeNumber); err != nil {
			return nil, err
		}
		if genresJoined != "" {
			e.Genres = strings.Split(genresJoined, ",")
		}
		results = append(results, e)
	}
	return results, rows.Err()
}

// GetPluginSecret returns the stored value and whether it exists.
func (m *DBManager) GetPluginSecret(pluginID, profileID, key string) (string, bool, error) {
	var v string
	err := m.db.QueryRow(`SELECT value FROM plugin_secrets WHERE plugin_id=? AND profile_id=? AND key=?`,
		pluginID, profileID, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetPluginSecret stores (or replaces) a secret.
func (m *DBManager) SetPluginSecret(pluginID, profileID, key, value string) error {
	_, err := m.db.Exec(`INSERT INTO plugin_secrets(plugin_id, profile_id, key, value) VALUES(?,?,?,?)
		ON CONFLICT(plugin_id, profile_id, key) DO UPDATE SET value=excluded.value`,
		pluginID, profileID, key, value)
	return err
}

// DeletePluginSecretsForProfile removes every plugin secret of a profile.
func (m *DBManager) DeletePluginSecretsForProfile(profileID string) error {
	_, err := m.db.Exec(`DELETE FROM plugin_secrets WHERE profile_id=?`, profileID)
	return err
}

// DeleteAllPluginSecrets removes every stored plugin secret.
func (m *DBManager) DeleteAllPluginSecrets() (int64, error) {
	res, err := m.db.Exec(`DELETE FROM plugin_secrets`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─────────────────────────────────────────────────────────────────────────────
// Raw SQL passthrough for internal packages.
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
