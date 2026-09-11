package main

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dailyLogFile writes to data/logs/<YYYY-MM-DD>.log, reopening the file the
// first time it's written to after the date rolls over. mycelium is a
// long-lived process, so without this every line since boot would land in the
// day-of-boot's file forever — the daily naming implied a rotation that never
// actually happened. Safe for concurrent use, though in practice log.Logger
// already serialises callers before reaching Write.
type dailyLogFile struct {
	mu   sync.Mutex
	dir  string
	day  string
	file *os.File
}

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
		}
	}
	if d.file == nil {
		return len(p), nil // no file yet (first open failed) — drop, don't block logging
	}
	return d.file.Write(p)
}
