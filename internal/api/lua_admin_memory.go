package api

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"time"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// memProfileMu serializes profiles: each one borrows every plugin's Lua
// states, two at once would only contend for them.
var memProfileMu sync.Mutex

// getLuaPluginMemory serves the per-plugin memory estimate
// (engine.MemoryProfile) next to the process's real figures: RSS, and the Go
// heap the plugins' share is part of.
func getLuaPluginMemory(w http.ResponseWriter, r *http.Request) {
	if !memProfileMu.TryLock() {
		http.Error(w, "profilo già in corso", http.StatusConflict)
		return
	}
	defer memProfileMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	start := time.Now()
	plugins := engine.LuaPlugins.MemoryProfile(ctx)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var luaTotal, indexTotal int64
	for _, p := range plugins {
		luaTotal += p.Bytes
		indexTotal += p.IndexBytes
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"rss_bytes":       managers.GetCoreStats().MemBytes,
		"go_heap_bytes":   ms.HeapAlloc,
		"go_heap_sys":     ms.HeapSys,
		"go_sys_bytes":    ms.Sys,
		"goroutines":      runtime.NumGoroutine(),
		"lua_total_bytes": luaTotal,
		"index_bytes":     indexTotal,
		"plugins":         plugins,
		"took_ms":         time.Since(start).Milliseconds(),
	})
}
