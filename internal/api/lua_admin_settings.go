package api

import (
	"encoding/json"
	"net/http"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// ─── list ─────────────────────────────────────────────────────────────────────

func listLuaPlugins(w http.ResponseWriter, r *http.Request) {
	metas := engine.LuaPlugins.GetMetaWithStatus()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metas)
}

// ─── settings ────────────────────────────────────────────────────────────────

func getLuaPluginSettings(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	metas := engine.LuaPlugins.GetMeta()

	var fields []engine.LuaSettingField
	for _, m := range metas {
		if m.ID == pluginID {
			fields = m.Settings.Global
			break
		}
	}

	result := make([]map[string]any, 0, len(fields))
	for _, f := range fields {
		val := managers.Settings.GetString("lua:"+pluginID+":global:"+f.ID, "")
		entry := map[string]any{
			"id":       f.ID,
			"label":    f.Label,
			"type":     f.Type,
			"required": f.Required,
		}
		if f.Type == "password" {
			entry["is_set"] = val != ""
		} else {
			entry["value"] = val
		}
		result = append(result, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

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
		if len(allowedKeys) > 0 && !allowedKeys[key] {
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
