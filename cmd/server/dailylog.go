package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// dailyLogFile writes to data/logs/<YYYY-MM-DD>.log, switching file on the
// first write after midnight and pruning old files (pruneOldDailyLogs).
// Safe for concurrent use.
type dailyLogFile struct {
	mu   sync.Mutex
	dir  string
	day  string
	file *os.File
	// sinceCheck counts bytes written since the last size check; dayBytes is
	// the size of today's file; capped means today's file hit logMaxDayBytes.
	sinceCheck, dayBytes int64
	capped               bool
}

// logRetentionDays is how long a rotated day file survives before
// pruneOldDailyLogs deletes it.
const logRetentionDays = 30

// Size limits for small disks: logMaxTotalBytes caps all day files together
// (oldest go first, never today's); logMaxDayBytes stops a runaway day
// (past it one notice is written; the dashboard's live log still has
// everything).
const (
	logMaxTotalBytes = 100 << 20
	logMaxDayBytes   = 50 << 20
	logCheckEvery    = 4 << 20 // re-check the total every 4 MB written
)

var dailyLogNamePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.log$`)

func newDailyLogFile(dir string) *dailyLogFile { return &dailyLogFile{dir: dir} }

func (d *dailyLogFile) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if today != d.day {
		// Open the new file before closing the old one: on failure keep writing to
		// the previous file.
		if f, err := os.OpenFile(filepath.Join(d.dir, today+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			old := d.file
			d.file, d.day = f, today
			if old != nil {
				old.Close()
			}
			d.dayBytes, d.capped = 0, false
			if fi, err := f.Stat(); err == nil {
				d.dayBytes = fi.Size()
			}
			pruneOldDailyLogs(d.dir, logRetentionDays, logMaxTotalBytes, today)
		}
	}
	if d.file == nil {
		return len(p), nil // no file yet: drop, don't block logging
	}
	if d.capped {
		return len(p), nil
	}
	if d.dayBytes+int64(len(p)) > logMaxDayBytes {
		d.capped = true
		_, _ = d.file.Write([]byte(time.Now().Format("2006/01/02 15:04:05") +
			" [log] file di oggi oltre il limite, righe successive non salvate su disco fino a domani (visibili nel log live della dashboard)\n"))
		return len(p), nil
	}
	n, err := d.file.Write(p)
	d.dayBytes += int64(n)
	d.sinceCheck += int64(n)
	if d.sinceCheck >= logCheckEvery {
		d.sinceCheck = 0
		pruneOldDailyLogs(d.dir, logRetentionDays, logMaxTotalBytes, d.day)
	}
	return n, err
}

// pruneOldDailyLogs deletes <YYYY-MM-DD>.log files older than retentionDays,
// then the oldest ones until the rest fit in maxTotal bytes. Today's file is
// never deleted, and only that exact name pattern is touched.
func pruneOldDailyLogs(dir string, retentionDays int, maxTotal int64, today string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	type dayFile struct {
		name string
		size int64
	}
	var kept []dayFile // ReadDir sorts by name = by date, oldest first
	var total int64
	for _, e := range entries {
		if e.IsDir() || !dailyLogNamePattern.MatchString(e.Name()) {
			continue
		}
		day, err := time.Parse("2006-01-02", e.Name()[:len("2006-01-02")])
		if err != nil {
			continue
		}
		if day.Before(cutoff) && e.Name() != today+".log" {
			os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		var size int64
		if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		kept = append(kept, dayFile{e.Name(), size})
		total += size
	}
	for _, f := range kept {
		if total <= maxTotal {
			break
		}
		if f.name == today+".log" {
			continue
		}
		if os.Remove(filepath.Join(dir, f.name)) == nil {
			total -= f.size
		}
	}
}
