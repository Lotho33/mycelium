// Admin endpoints for Lua plugin management — split across files by concern
// (same P2-2 treatment as admin.go/proxy.go): lua_admin.go (this file, route
// table only), lua_admin_tasks.go (manual task run + start/stop/restart),
// lua_admin_settings.go (list + per-plugin settings), lua_admin_upload.go
// (ZIP install/uninstall), lua_admin_logs.go (log buffer + SSE + stats),
// lua_admin_files.go (in-dashboard file editor), lua_admin_helpers.go (shared
// small helpers). Pure code move, no behaviour or package boundary changed.
package api

import "net/http"

// RegisterLuaAdminRoutes registers admin endpoints for Lua plugin management.
func RegisterLuaAdminRoutes(mux *http.ServeMux) {
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/lua-plugins", auth(listLuaPlugins))
	mux.HandleFunc("GET /admin/lua-plugins/settings/{plugin_id}", auth(getLuaPluginSettings))
	mux.HandleFunc("POST /admin/lua-plugins/settings/{plugin_id}", auth(saveLuaPluginSettings))
	mux.HandleFunc("POST /admin/lua-plugins/run-task/{plugin_id}/{task}", auth(runLuaTask))
	mux.HandleFunc("POST /admin/lua-plugins/state/{plugin_id}", auth(setLuaPluginState))
	mux.HandleFunc("POST /admin/lua-plugins/upload", auth(uploadLuaPlugin))
	mux.HandleFunc("DELETE /admin/lua-plugins/uninstall", auth(uninstallLuaPlugin))
	// Same rate-limit as the other password-gated destructive actions
	// (admin_wipe.go) — a stolen admin session cookie without the real
	// password shouldn't get unlimited guesses here either.
	mux.HandleFunc("POST /admin/lua-plugins/wipe-data/{plugin_id}", rateLimitMiddleware(wipeLimiter, auth(wipePluginDataFor)))
	mux.HandleFunc("GET /admin/lua-plugins/logs", auth(getLuaPluginLogs))
	mux.HandleFunc("GET /admin/lua-plugins/logs/stream", auth(getLuaPluginLogsSSE))
	mux.HandleFunc("GET /admin/lua-plugins/stats", auth(getLuaPluginStats))
	mux.HandleFunc("GET /admin/lua-plugins/files/{plugin_id}", auth(listLuaPluginFiles))
	mux.HandleFunc("GET /admin/lua-plugins/file/{plugin_id}", auth(getLuaPluginFile))
	mux.HandleFunc("PUT /admin/lua-plugins/file/{plugin_id}", auth(putLuaPluginFile))
}
