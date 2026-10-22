package api

import (
	"encoding/json"
	"fmt"
	"net/http"

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
	buf := engine.LuaPlugins.GetLogBuffer(pluginID)
	if buf == nil {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: (nessun log per %s)\n\n", sseLine.Replace(pluginID))
		return
	}
	streamLogSSE(w, r, buf.Snapshot)
}
