package managers

import (
	"bytes"
	"database/sql"
	"log"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// watchHistorySchema mirrors the watch_history table as it looks after every
// migration in InitDB has been applied (base CREATE TABLE + all ALTER TABLE
// ADD COLUMN statements) — the shape UpsertProgress/GetContinueWatching are
// written against.
const watchHistorySchema = `
CREATE TABLE watch_history (
	client_id TEXT,
	provider_id TEXT,
	playable_id TEXT,
	title TEXT,
	poster TEXT,
	progress_time REAL,
	total_time REAL,
	is_completed BOOLEAN,
	last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
	parent_id TEXT NOT NULL DEFAULT '',
	navigation_context TEXT NOT NULL DEFAULT '',
	rating REAL NOT NULL DEFAULT 0,
	genres TEXT NOT NULL DEFAULT '',
	plot TEXT NOT NULL DEFAULT '',
	year INTEGER NOT NULL DEFAULT 0,
	logo_url TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (client_id, provider_id, playable_id)
);
`

func newTestDB(t *testing.T, extraSchema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(watchHistorySchema); err != nil {
		t.Fatalf("create watch_history: %v", err)
	}
	if extraSchema != "" {
		if _, err := db.Exec(extraSchema); err != nil {
			t.Fatalf("create extra schema: %v", err)
		}
	}
	return db
}

// --- Bug 1: migration error classification -------------------------------

// captureLog redirects the standard logger's output for the duration of fn
// and returns everything written to it, restoring the previous output
// afterwards. Standard pattern for asserting on log.Printf output in Go
// tests.
func captureLog(fn func()) string {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

func TestIsDuplicateColumnError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{
			"exact modernc.org/sqlite v1.48.2 message",
			// Verified directly against the driver: re-running
			// `ALTER TABLE t ADD COLUMN foo TEXT` on a table that
			// already has `foo` returns exactly this message.
			errStr("SQL logic error: duplicate column name: foo (1)"),
			true,
		},
		{"different column name", errStr("SQL logic error: duplicate column name: parent_id (1)"), true},
		{"different case", errStr("SQL LOGIC ERROR: DUPLICATE COLUMN NAME: foo (1)"), true},
		{
			"unrelated sqlite error (NOT NULL without default)",
			// Verified against the driver: `ALTER TABLE t ADD COLUMN
			// bar TEXT NOT NULL` on a non-empty table returns this.
			errStr("SQL logic error: Cannot add a NOT NULL column with default value NULL (1)"),
			false,
		},
		{"disk full", errStr("SQL logic error: database or disk is full (13)"), false},
		{"database corrupt", errStr("SQL logic error: database disk image is malformed (11)"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDuplicateColumnError(c.err); got != c.want {
				t.Errorf("isDuplicateColumnError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// errStr is a trivial error for table-driven tests.
type errStr string

func (e errStr) Error() string { return string(e) }

// TestRunMigration_DuplicateColumnIsSilent verifies the pre-existing,
// intended behavior: a real "duplicate column" error from the driver is
// swallowed without logging anything.
func TestRunMigration_DuplicateColumnIsSilent(t *testing.T) {
	db := newTestDB(t, `CREATE TABLE probe (id INTEGER PRIMARY KEY)`)
	if _, err := db.Exec(`ALTER TABLE probe ADD COLUMN foo TEXT`); err != nil {
		t.Fatalf("initial ALTER TABLE: %v", err)
	}

	out := captureLog(func() {
		// Re-running the same ADD COLUMN reproduces the real "already
		// migrated in a previous boot" case handled by runMigration.
		runMigration(db, `ALTER TABLE probe ADD COLUMN foo TEXT`)
	})
	if out != "" {
		t.Errorf("runMigration logged on a duplicate-column error, want silence; got: %q", out)
	}
}

// TestRunMigration_OtherErrorIsLogged is the Bug 1 regression test: an
// unexpected migration error (anything other than "duplicate column") must
// produce a visible warning instead of being silently discarded.
func TestRunMigration_OtherErrorIsLogged(t *testing.T) {
	db := newTestDB(t, `
		CREATE TABLE probe (id INTEGER PRIMARY KEY);
		INSERT INTO probe (id) VALUES (1);
	`)

	out := captureLog(func() {
		// Adding a NOT NULL column with no default to a non-empty table
		// is rejected by SQLite with an error distinct from "duplicate
		// column name" (verified against the driver) — this must be
		// logged, not ignored.
		runMigration(db, `ALTER TABLE probe ADD COLUMN bar TEXT NOT NULL`)
	})
	if out == "" {
		t.Fatal("runMigration silently swallowed a non-duplicate-column error, want a logged warning")
	}
	if !strings.Contains(out, "migrazione DB fallita") {
		t.Errorf("logged output missing expected warning text, got: %q", out)
	}
}

// --- Bug 2: UpsertProgress DELETE+INSERT atomicity ------------------------

func TestUpsertProgress_ReplacesSeriesEntryAtomically(t *testing.T) {
	db := newTestDB(t, "")
	m := NewDBManager(db)

	const clientID, providerID, parentID = "client1", "prov1", "series1"

	if err := m.UpsertProgress(clientID, providerID, "ep1", parentID, "Series", "Ep1", "poster1", 100, 1000, 0, nil, "", 0); err != nil {
		t.Fatalf("first UpsertProgress: %v", err)
	}
	if err := m.UpsertProgress(clientID, providerID, "ep2", parentID, "Series", "Ep2", "poster2", 200, 1000, 0, nil, "", 0); err != nil {
		t.Fatalf("second UpsertProgress: %v", err)
	}

	rows, err := db.Query(`SELECT playable_id FROM watch_history WHERE client_id=? AND provider_id=? AND parent_id=?`, clientID, providerID, parentID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(ids) != 1 || ids[0] != "ep2" {
		t.Fatalf("want exactly one row (ep2) for the series after the second upsert, got %v", ids)
	}
}

// TestUpsertProgress_RollsBackOnInsertFailure forces the INSERT half of the
// transaction to fail (via a trigger) and verifies the preceding DELETE was
// rolled back too — the old row must still be present, not silently gone.
func TestUpsertProgress_RollsBackOnInsertFailure(t *testing.T) {
	const trigger = `
		CREATE TRIGGER fail_on_marker BEFORE INSERT ON watch_history
		WHEN NEW.title = 'FAIL_TRIGGER'
		BEGIN
			SELECT RAISE(ABORT, 'forced failure for test');
		END;
	`
	db := newTestDB(t, trigger)
	m := NewDBManager(db)

	const clientID, providerID, parentID = "client1", "prov1", "series1"

	if err := m.UpsertProgress(clientID, providerID, "ep1", parentID, "Series", "Ep1", "poster1", 100, 1000, 0, nil, "", 0); err != nil {
		t.Fatalf("initial UpsertProgress: %v", err)
	}

	err := m.UpsertProgress(clientID, providerID, "ep2", parentID, "Series", "FAIL_TRIGGER", "poster2", 200, 1000, 0, nil, "", 0)
	if err == nil {
		t.Fatal("expected UpsertProgress to fail when the insert trigger aborts, got nil error")
	}

	rows, err := db.Query(`SELECT playable_id FROM watch_history WHERE client_id=? AND provider_id=? AND parent_id=?`, clientID, providerID, parentID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(ids) != 1 || ids[0] != "ep1" {
		t.Fatalf("rollback failed: want the original row (ep1) untouched, got %v", ids)
	}
}

// --- Server-observed position snapshot (StreamSession.RecordFetch) -------

// TestUpdateProgressPosition_NeverCreatesARow is the regression test for the
// blank "ghost" Continue Watching card: a segment-fetch snapshot for an id
// with no existing row (e.g. the resolved stream id diverged from the id the
// client's own UpdateProgress heartbeat used) must be a silent no-op, never
// an insert.
func TestUpdateProgressPosition_NeverCreatesARow(t *testing.T) {
	db := newTestDB(t, "")
	m := NewDBManager(db)

	if err := m.UpdateProgressPosition("client1", "prov1", "ghost-id", 50, 1000); err != nil {
		t.Fatalf("UpdateProgressPosition: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM watch_history WHERE client_id='client1' AND provider_id='prov1' AND playable_id='ghost-id'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("want no row created for an id with no existing entry, got %d", count)
	}
}

// TestUpdateProgressPosition_UpdatesPositionOnly verifies it refines an
// existing row's position/is_completed without touching (or being able to
// touch) title/poster/plot/parent_id — those stay whatever the client's own
// UpdateProgress last set.
func TestUpdateProgressPosition_UpdatesPositionOnly(t *testing.T) {
	db := newTestDB(t, "")
	m := NewDBManager(db)

	const clientID, providerID, parentID = "client1", "prov1", "series1"
	if err := m.UpsertProgress(clientID, providerID, "ep1", parentID, "Series", "Ep1", "poster1", 100, 1000, 4.5, []string{"Drama"}, "The plot.", 2024); err != nil {
		t.Fatalf("seed UpsertProgress: %v", err)
	}

	if err := m.UpdateProgressPosition(clientID, providerID, "ep1", 950, 1000); err != nil {
		t.Fatalf("UpdateProgressPosition: %v", err)
	}

	var title, poster, plot, pid string
	var progress float64
	var completed bool
	err := db.QueryRow(`SELECT title, poster, plot, parent_id, progress_time, is_completed FROM watch_history WHERE client_id=? AND provider_id=? AND playable_id=?`,
		clientID, providerID, "ep1").Scan(&title, &poster, &plot, &pid, &progress, &completed)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if title != "Ep1" || poster != "poster1" || plot != "The plot." || pid != parentID {
		t.Fatalf("metadata must be untouched, got title=%q poster=%q plot=%q parent_id=%q", title, poster, plot, pid)
	}
	if progress != 950 {
		t.Fatalf("want progress_time updated to 950, got %v", progress)
	}
	if !completed {
		t.Fatalf("want is_completed=true at 95%% progress")
	}
}

// --- Continue-watching logo (background fetch gate + write) --------------

func TestNeedsLogo_TrueWhenEmptyFalseOnceSet(t *testing.T) {
	db := newTestDB(t, "")
	m := NewDBManager(db)

	target := WatchHistoryLogoTarget{ClientID: "c1", ProviderID: "prov1", ParentID: "series1", PlayableID: "ep1"}

	if err := m.UpsertProgress(target.ClientID, target.ProviderID, target.PlayableID, target.ParentID, "Series", "Ep1", "poster1", 100, 1000, 0, nil, "", 0); err != nil {
		t.Fatalf("UpsertProgress: %v", err)
	}

	needs, err := m.NeedsLogo(target)
	if err != nil {
		t.Fatalf("NeedsLogo: %v", err)
	}
	if !needs {
		t.Fatal("expected NeedsLogo=true for a freshly inserted row with no logo yet")
	}

	if err := m.UpdateWatchHistoryLogo(target, "https://example.com/logo.png"); err != nil {
		t.Fatalf("UpdateWatchHistoryLogo: %v", err)
	}

	needs, err = m.NeedsLogo(target)
	if err != nil {
		t.Fatalf("NeedsLogo after update: %v", err)
	}
	if needs {
		t.Fatal("expected NeedsLogo=false once a logo_url has been set")
	}
}

// TestUpdateWatchHistoryLogo_NeverOverwritesExisting: a second, different
// logo_url must not clobber one already stored — the SQL guard (only touch
// a row whose logo_url is still an empty string) is what NeedsLogo relies on
// to fire only once per show.
func TestUpdateWatchHistoryLogo_NeverOverwritesExisting(t *testing.T) {
	db := newTestDB(t, "")
	m := NewDBManager(db)
	target := WatchHistoryLogoTarget{ClientID: "c1", ProviderID: "prov1", ParentID: "series1", PlayableID: "ep1"}

	if err := m.UpsertProgress(target.ClientID, target.ProviderID, target.PlayableID, target.ParentID, "Series", "Ep1", "poster1", 100, 1000, 0, nil, "", 0); err != nil {
		t.Fatalf("UpsertProgress: %v", err)
	}
	if err := m.UpdateWatchHistoryLogo(target, "https://example.com/first.png"); err != nil {
		t.Fatalf("first UpdateWatchHistoryLogo: %v", err)
	}
	if err := m.UpdateWatchHistoryLogo(target, "https://example.com/second.png"); err != nil {
		t.Fatalf("second UpdateWatchHistoryLogo: %v", err)
	}

	entries, err := m.GetContinueWatching(target.ClientID, 10)
	if err != nil {
		t.Fatalf("GetContinueWatching: %v", err)
	}
	if len(entries) != 1 || entries[0].LogoURL != "https://example.com/first.png" {
		t.Fatalf("expected the first logo_url to stick, got %+v", entries)
	}
}

// TestUpdateWatchHistoryLogo_MovieHasNoParent covers the parentID=="" path
// (a movie, targeted by playable_id instead of parent_id).
func TestUpdateWatchHistoryLogo_MovieHasNoParent(t *testing.T) {
	db := newTestDB(t, "")
	m := NewDBManager(db)
	target := WatchHistoryLogoTarget{ClientID: "c1", ProviderID: "prov1", ParentID: "", PlayableID: "movie1"}

	if err := m.UpsertProgress(target.ClientID, target.ProviderID, target.PlayableID, "", "", "Movie", "poster1", 100, 1000, 0, nil, "", 0); err != nil {
		t.Fatalf("UpsertProgress: %v", err)
	}

	needs, err := m.NeedsLogo(target)
	if err != nil || !needs {
		t.Fatalf("NeedsLogo = %v, %v; want true, nil", needs, err)
	}
	if err := m.UpdateWatchHistoryLogo(target, "https://example.com/movie-logo.png"); err != nil {
		t.Fatalf("UpdateWatchHistoryLogo: %v", err)
	}

	entries, err := m.GetContinueWatching(target.ClientID, 10)
	if err != nil {
		t.Fatalf("GetContinueWatching: %v", err)
	}
	if len(entries) != 1 || entries[0].LogoURL != "https://example.com/movie-logo.png" {
		t.Fatalf("expected the movie's logo_url to be set, got %+v", entries)
	}
}
