package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDailyLogFileWritesToTodayFile(t *testing.T) {
	dir := t.TempDir()
	d := newDailyLogFile(dir)

	if _, err := d.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	today := time.Now().Format("2006-01-02")
	data, err := os.ReadFile(filepath.Join(dir, today+".log"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("unexpected content: %q", data)
	}
}

func TestPruneOldDailyLogsDeletesOnlyOldDailyFiles(t *testing.T) {
	dir := t.TempDir()

	old := time.Now().AddDate(0, 0, -31).Format("2006-01-02") + ".log"
	recent := time.Now().AddDate(0, 0, -5).Format("2006-01-02") + ".log"
	unrelated := "jellyfin.log" // a plugin's own log file, must survive untouched

	for _, name := range []string{old, recent, unrelated} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}

	pruneOldDailyLogs(dir, logRetentionDays)

	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Errorf("expected %s to be deleted, err=%v", old, err)
	}
	if _, err := os.Stat(filepath.Join(dir, recent)); err != nil {
		t.Errorf("expected %s to survive, got err=%v", recent, err)
	}
	if _, err := os.Stat(filepath.Join(dir, unrelated)); err != nil {
		t.Errorf("expected unrelated file %s to survive untouched, got err=%v", unrelated, err)
	}
}

func TestDailyLogFileRotatesAndPrunesOnDayChange(t *testing.T) {
	dir := t.TempDir()
	d := newDailyLogFile(dir)

	oldDay := "2000-01-01"
	if err := os.WriteFile(filepath.Join(dir, oldDay+".log"), []byte("ancient"), 0644); err != nil {
		t.Fatalf("seed old file: %v", err)
	}

	// Force the internal "current day" to something stale so the next Write
	// triggers the rollover-and-prune path, same as a real day change would.
	d.day = "1999-12-31"

	if _, err := d.Write([]byte("new day\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, oldDay+".log")); !os.IsNotExist(err) {
		t.Errorf("expected stale day file to be pruned on rollover, err=%v", err)
	}
}
