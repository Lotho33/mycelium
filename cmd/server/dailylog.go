package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// dailyLogFile writes to data/logs/<YYYY-MM-DD>.log, reopening the file the
// first time it's written to after the date rolls over, and pruning files
// older than logRetentionDays at the same time. mycelium is a long-lived
// process with nobody watching disk usage on it (see deploy-appliance-node.md)
// — without both of these, every line since boot would land in the
// day-of-boot's file forever, and the day files that DO get rotated would
// then just accumulate on disk without limit forever too. Safe for
// concurrent use, though in practice log.Logger already serialises callers
// before reaching Write.
type dailyLogFile struct {
	mu   sync.Mutex
	dir  string
	day  string
	file *os.File
}

// logRetentionDays is how long a rotated day file survives before
// pruneOldDailyLogs deletes it.
const logRetentionDays = 30

var dailyLogNamePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.log$`)

func newDailyLogFile(dir string) *dailyLogFile { return &dailyLogFile{dir: dir} }

func (d *dailyLogFile) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if today != d.day {
		// Open the new day's file before touching the old one: on failure we
		// keep writing to the previous file instead of losing every line
		// until a rotation eventually succeeds.
		if f, err := os.OpenFile(filepath.Join(d.dir, today+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			old := d.file
			d.file, d.day = f, today
			if old != nil {
				old.Close()
			}
			pruneOldDailyLogs(d.dir, logRetentionDays)
		}
	}
	if d.file == nil {
		return len(p), nil // no file yet (first open failed) — drop, don't block logging
	}
	return d.file.Write(p)
}

// pruneOldDailyLogs deletes dailyLogFile's own <YYYY-MM-DD>.log files older
// than retentionDays. Only ever touches names matching that exact pattern,
// so other files sharing the same directory (e.g. a plugin's own log) are
// never at risk. Best-effort: a read or remove failure just leaves that file
// for the next day's rollover to retry.
func pruneOldDailyLogs(dir string, retentionDays int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	for _, e := range entries {
		if e.IsDir() || !dailyLogNamePattern.MatchString(e.Name()) {
			continue
		}
		day, err := time.Parse("2006-01-02", e.Name()[:len("2006-01-02")])
		if err != nil || !day.Before(cutoff) {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
}
