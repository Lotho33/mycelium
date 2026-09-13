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
