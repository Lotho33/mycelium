package managers

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"mycelium/internal/core"
)

const (
	pluginLogMaxBytes = 2 * 1024 * 1024 // 2 MB per file
	pluginLogMaxFiles = 3               // keep .log + 2 rotated
)

// PluginLogBuffer mantiene gli ultimi N log di un singolo plugin in RAM
// e li scrive anche su file con rotazione a 2 MB.
type PluginLogBuffer struct {
	lines []string
	total int64 // monotonically increasing, never wraps
	mu    sync.Mutex
	max   int

	fileMu  sync.Mutex
	logFile *os.File
	logPath string
	written int64
}

func newPluginLogBuffer(pluginID string, maxLines int) *PluginLogBuffer {
	b := &PluginLogBuffer{max: maxLines}
	logDir := core.AppPath("data", "logs")
	if err := os.MkdirAll(logDir, 0755); err == nil {
		b.logPath = filepath.Join(logDir, pluginID+".log")
		b.openLogFile()
	}
	return b
}

func (b *PluginLogBuffer) openLogFile() {
	f, err := os.OpenFile(b.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return
	}
	b.logFile = f
	b.written = info.Size()
}

func (b *PluginLogBuffer) rotate() {
	if b.logFile != nil {
		b.logFile.Close()
		b.logFile = nil
	}
	// Shift rotated files: .log.2 → removed, .log.1 → .log.2, .log → .log.1
	for i := pluginLogMaxFiles - 1; i >= 1; i-- {
		older := fmt.Sprintf("%s.%d", b.logPath, i)
		newer := fmt.Sprintf("%s.%d", b.logPath, i-1)
		if i == 1 {
			newer = b.logPath
		}
		os.Rename(newer, older)
	}
	b.written = 0
	b.openLogFile()
}

func (b *PluginLogBuffer) Append(line string) {
	b.mu.Lock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.lines = b.lines[1:]
	}
	b.total++
	b.mu.Unlock()

	b.fileMu.Lock()
	defer b.fileMu.Unlock()
	if b.logFile == nil {
		return
	}
	data := []byte(line + "\n")
	if b.written+int64(len(data)) > pluginLogMaxBytes {
		b.rotate()
	}
	if b.logFile != nil {
		n, _ := b.logFile.Write(data)
		b.written += int64(n)
	}
}

func (b *PluginLogBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

// Snapshot returns a copy of all lines and the current total count.
func (b *PluginLogBuffer) Snapshot() (lines []string, total int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines = make([]string, len(b.lines))
	copy(lines, b.lines)
	return lines, b.total
}

func (b *PluginLogBuffer) Close() {
	b.fileMu.Lock()
	defer b.fileMu.Unlock()
	if b.logFile != nil {
		b.logFile.Close()
		b.logFile = nil
	}
}

// NewPluginLogBuffer is the exported constructor for use outside the managers package.
func NewPluginLogBuffer(pluginID string, maxLines int) *PluginLogBuffer {
	return newPluginLogBuffer(pluginID, maxLines)
}
