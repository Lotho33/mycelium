package api

import (
	"encoding/json"
	"net/http"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// ─── settings ────────────────────────────────────────────────────────────────

func saveLuaPluginSettings(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")

	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	metas := engine.LuaPlugins.GetMeta()
	var fields []engine.LuaSettingField
	for _, m := range metas {
		if m.ID == pluginID {
			fields = m.Settings.Global
			break
		}
	}
	allowedKeys := make(map[string]bool, len(fields))
	for _, f := range fields {
		allowedKeys[f.ID] = true
	}

	toSave := make(map[string]any, len(body))
	for key, val := range body {
		if !allowedKeys[key] {
			continue // silently skip keys not declared by this plugin's manifest
		}
		toSave["lua:"+pluginID+":global:"+key] = val
	}
	if err := managers.Settings.Save(toSave); err != nil {
		http.Error(w, "save error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
