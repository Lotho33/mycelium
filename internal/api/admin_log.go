package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// --- SISTEMA DI LOG IN MEMORIA ---
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

func getLogs(w http.ResponseWriter, r *http.Request) {
	UILogs.mu.RLock()
	defer UILogs.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"logs": UILogs.lines})
}

// getLogsSSE godoc
//
//	@Summary		Stream log SSE
//	@Description	Invia i log del core in tempo reale via Server-Sent Events
//	@Tags			Admin
//	@Produce		text/event-stream
//	@Success		200	{string}	string	"stream SSE"
//	@Router			/admin/logs/stream [get]
func getLogsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	existing, lastTotal := UILogs.snapshot()
	for _, line := range existing {
		fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", " "))
	}
	flusher.Flush()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			lines, total := UILogs.snapshot()
			newCount := total - lastTotal
			if newCount > 0 {
				// Take the last newCount lines; some may be evicted if buffer wrapped.
				start := max(len(lines)-int(newCount), 0)
				for _, line := range lines[start:] {
					fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", " "))
				}
			}
			lastTotal = total
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
