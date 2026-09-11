package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"mycelium/internal/engine"
)

// ─── logs ─────────────────────────────────────────────────────────────────────

func getLuaPluginLogs(w http.ResponseWriter, r *http.Request) {
	pluginID := r.URL.Query().Get("plugin_id")
	if pluginID == "" {
		http.Error(w, "plugin_id mancante", http.StatusBadRequest)
		return
	}
	buf := engine.LuaPlugins.GetLogBuffer(pluginID)
	lines := []string{}
	if buf != nil {
		lines = buf.Lines()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"logs": lines})
}

func getLuaPluginLogsSSE(w http.ResponseWriter, r *http.Request) {
	pluginID := r.URL.Query().Get("plugin_id")
	if pluginID == "" {
		http.Error(w, "plugin_id mancante", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	buf := engine.LuaPlugins.GetLogBuffer(pluginID)
	if buf == nil {
		fmt.Fprintf(w, "data: (nessun log per %s)\n\n", pluginID)
		flusher.Flush()
		return
	}

	existing, lastTotal := buf.Snapshot()
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
			lines, total := buf.Snapshot()
			newCount := total - lastTotal
			if newCount > 0 {
				start := max(int64(len(lines))-newCount, 0)
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

// ─── stats ────────────────────────────────────────────────────────────────────

// getLuaPluginStats returns lightweight in-process stats for all loaded Lua plugins.
// Response: {"plugin_id": {"log_lines": N, "loaded": true}, ...}
func getLuaPluginStats(w http.ResponseWriter, r *http.Request) {
	type pluginStat struct {
		LogLines int  `json:"log_lines"`
		Loaded   bool `json:"loaded"`
	}
	result := map[string]pluginStat{}
	for _, mf := range engine.LuaPlugins.GetMeta() {
		buf := engine.LuaPlugins.GetLogBuffer(mf.ID)
		lines := 0
		if buf != nil {
			lines = len(buf.Lines())
		}
		result[mf.ID] = pluginStat{
			LogLines: lines,
			Loaded:   engine.LuaPlugins.Has(mf.ID),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
