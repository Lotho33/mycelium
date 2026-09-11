package api

import (
	"encoding/json"
	"log"
	"net/http"

	"mycelium/internal/engine"
)

// ─── task runner ──────────────────────────────────────────────────────────────

func runLuaTask(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	task := r.PathValue("task")

	// Prima bypassava del tutto bgTaskSem — un click "Esegui" poteva girare
	// in parallelo con QUALSIASI numero di task schedulati pesanti, vanificando
	// il motivo per cui il semaforo esiste. Tentativo NON bloccante: se occupato,
	// risposta immediata (nessuna attesa silenziosa per la durata di un sync
	// altrui) con un errore chiaro invece di far partire comunque il task.
	if !engine.TryAcquireBgTaskSem() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"ok":false,"message":"un altro task è già in corso, riprova tra poco"}`))
		return
	}
	// bgTaskSem da solo (capacità 2) non basta a impedire che questo run
	// manuale parta mentre un tick cron dello STESSO plugin è già a metà —
	// serve anche il lock per-plugin, stesso motivo/stesso pattern di
	// TriggerLiveRefresh (vedi TryAcquirePluginTaskLock in lua_plugin.go).
	releaseTaskLock, gotLock := engine.LuaPlugins.TryAcquirePluginTaskLock(pluginID)
	if !gotLock {
		engine.ReleaseBgTaskSem()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"ok":false,"message":"un task di questo plugin è già in corso, riprova tra poco"}`))
		return
	}

	go func() {
		defer engine.ReleaseBgTaskSem()
		defer releaseTaskLock()
		// Goroutine nuda: un panic qui (non un semplice errore Lua, quello
		// è già un ritorno normale via err) crasherebbe l'intero processo
		// senza questo recover — stesso pattern/stessa causa dei crash
		// visti durante i task schedulati pesanti, vedi il commento su
		// runTasks in lua_plugin.go.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[lua] manual task %s/%s: PANIC recuperato: %v", pluginID, task, r)
			}
		}()
		_, err := engine.LuaPlugins.CallEntrypoint(pluginID, task, nil, "")
		engine.LuaPlugins.RecordTaskResult(pluginID, err == nil)
		if err != nil {
			log.Printf("[lua] manual task %s/%s: %v", pluginID, task, err)
		} else {
			log.Printf("[lua] manual task %s/%s: OK", pluginID, task)
			engine.MarkTaskRan(pluginID, task)
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"message":"task started in background"}`))
}

// ─── lifecycle (start / stop / restart) ──────────────────────────────────────

// setLuaPluginState drives a plugin's operational lifecycle from the dashboard.
// Body: {"action": "start" | "stop" | "restart"}.
//   - start   : allowed only when no required setting is unset
//   - stop    : always allowed
//   - restart : reloads manifest + scripts from disk; only on a running plugin
func setLuaPluginState(w http.ResponseWriter, r *http.Request) {
	pluginID := r.PathValue("plugin_id")
	if !engine.LuaPlugins.Has(pluginID) {
		http.Error(w, "plugin non trovato", http.StatusNotFound)
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "JSON non valido", http.StatusBadRequest)
		return
	}

	switch body.Action {
	case "start":
		if missing := engine.LuaPlugins.MissingRequiredSettings(pluginID); len(missing) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "message": "configurazione incompleta", "missing_required": missing,
			})
			return
		}
		engine.LuaPlugins.SetRunEnabled(pluginID, true)
	case "stop":
		engine.LuaPlugins.SetRunEnabled(pluginID, false)
	case "restart":
		if !engine.LuaPlugins.IsOperational(pluginID) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "message": "riavvio possibile solo su un plugin attivo",
			})
			return
		}
		dir, err := findLuaPluginDir(pluginID)
		if err != nil {
			http.Error(w, "directory plugin non trovata", http.StatusNotFound)
			return
		}
		engine.LuaPlugins.UnloadPlugin(pluginID)
		if err := engine.LuaPlugins.LoadPlugin(dir); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "message": "reload fallito: " + err.Error(),
			})
			return
		}
	default:
		http.Error(w, "azione non valida (start|stop|restart)", http.StatusBadRequest)
		return
	}

	log.Printf("[lua] plugin %s: %s → %s", pluginID, body.Action, engine.LuaPlugins.RunStateOf(pluginID))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "run_state": engine.LuaPlugins.RunStateOf(pluginID),
	})
}
