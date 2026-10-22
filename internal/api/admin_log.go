package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// --- in-memory log buffer ---
type LogBuffer struct {
	lines   []string
	total   int64 // monotonic append count — never wraps in practice
	mu      sync.RWMutex
	max     int
	fileOut io.Writer // optional file sink (set via SetFileOutput)
}

var UILogs = &LogBuffer{max: 300}

// SetFileOutput attaches a file writer; all subsequent log lines are also written there.
func (l *LogBuffer) SetFileOutput(w io.Writer) {
	l.mu.Lock()
	l.fileOut = w
	l.mu.Unlock()
}

func (l *LogBuffer) Write(p []byte) (n int, err error) {
	line := strings.TrimSpace(string(p))
	if line == "" {
		return len(p), nil
	}
	if len(line) == 0 || line[0] < '0' || line[0] > '9' {
		line = time.Now().Format("2006/01/02 15:04:05") + " " + line
	}
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.total++
	if len(l.lines) > l.max {
		l.lines = l.lines[1:]
	}
	fw := l.fileOut
	l.mu.Unlock()
	if fw != nil {
		fw.Write([]byte(line + "\n"))
	}
	return os.Stdout.Write(p)
}

func (l *LogBuffer) Append(line string) {
	if line == "" {
		return
	}
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.total++
	if len(l.lines) > l.max {
		l.lines = l.lines[1:]
	}
	l.mu.Unlock()
}

// snapshot returns a copy of current lines and the monotonic total append count.
func (l *LogBuffer) snapshot() (lines []string, total int64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]string(nil), l.lines...), l.total
}

// getLogsSSE streams the core log live as Server-Sent Events.
func getLogsSSE(w http.ResponseWriter, r *http.Request) {
	streamLogSSE(w, r, UILogs.snapshot)
}

// sseLine makes a log line one SSE data field: a CR or LF would end it and
// let the rest (plugin-controlled text) inject fields of its own.
var sseLine = strings.NewReplacer("\r", " ", "\n", " ")

// streamLogSSE sends snapshot's lines, then every new one each second, until
// the client leaves or the admin session stops being valid.
func streamLogSSE(w http.ResponseWriter, r *http.Request, snapshot func() ([]string, int64)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	existing, lastTotal := snapshot()
	for _, line := range existing {
		fmt.Fprintf(w, "data: %s\n\n", sseLine.Replace(line))
	}
	flusher.Flush()

	cookie, _ := r.Cookie(adminSessionCookie)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for tick := 1; ; tick++ {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// A logged-out or expired session stops receiving lines.
			if tick%20 == 0 && (cookie == nil || !isValidAdminSession(cookie.Value)) {
				return
			}
			lines, total := snapshot()
			if newCount := total - lastTotal; newCount > 0 {
				// The last newCount lines; some may be gone if the buffer wrapped.
				start := max(int64(len(lines))-newCount, 0)
				for _, line := range lines[start:] {
					fmt.Fprintf(w, "data: %s\n\n", sseLine.Replace(line))
				}
			}
			lastTotal = total
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
