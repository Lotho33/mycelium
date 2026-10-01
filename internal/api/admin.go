// Package api's /admin/* surface: admin.go (route table), admin_session.go
// (cookie auth), admin_auth_pages.go (login/logout/dashboard), admin_log.go
// (log buffer + SSE), admin_clients.go (hub API clients), admin_wipe.go
// (destructive actions), admin_system.go (settings, status, info),
// admin_password.go (password change).
package api

import "net/http"

func AdminRoutes(mux *http.ServeMux) {
	// Login/logout: public (POST rate limited).
	mux.HandleFunc("GET /admin/login", adminLoginPage)
	mux.HandleFunc("POST /admin/login", rateLimitMiddleware(loginLimiter, adminLoginSubmit))
	mux.HandleFunc("POST /admin/logout", adminLogout)

	// Everything else requires a valid session.
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/dashboard", auth(adminDashboard))
	mux.HandleFunc("GET /admin/logs", auth(getLogs))

	mux.HandleFunc("GET /admin/clients", auth(listClients))
	mux.HandleFunc("POST /admin/clients", auth(addClient))
	mux.HandleFunc("DELETE /admin/clients/{client_id}", auth(removeClient))
	mux.HandleFunc("GET /admin/dev-token", auth(generateDevToken))

	// Lua plugin management lives under /admin/lua-plugins/* (RegisterLuaAdminRoutes).

	mux.HandleFunc("POST /admin/settings/save", auth(saveSettings))
	// Rate limited: it checks the current password.
	mux.HandleFunc("POST /admin/password/change", rateLimitMiddleware(wipeLimiter, auth(changeAdminPasswordHandler)))
	mux.HandleFunc("POST /admin/cache/clear", auth(clearCacheHandler))
	// Rate limited like login: they re-check the admin password.
	mux.HandleFunc("POST /admin/profiles/wipe", rateLimitMiddleware(wipeLimiter, auth(wipeProfilesHandler)))
	mux.HandleFunc("POST /admin/plugins/wipe-data", rateLimitMiddleware(wipeLimiter, auth(wipePluginDataHandler)))
	mux.HandleFunc("POST /admin/factory-reset", rateLimitMiddleware(wipeLimiter, auth(factoryResetHandler)))
	mux.HandleFunc("GET /admin/core/resources", auth(getCoreResources))
	mux.HandleFunc("GET /admin/plugins/info", auth(getPluginsInfo))
	mux.HandleFunc("GET /admin/enricher-bindings", auth(getEnricherBindings))
	mux.HandleFunc("GET /admin/info", auth(getAdminInfo))
	// Generic egress proxy URL (VPN_PROXY_URL is the primary knob).
	mux.HandleFunc("POST /admin/vpn", auth(setVPNProxy))
	mux.HandleFunc("POST /admin/vpn/test", auth(testVPNConnection))
	mux.HandleFunc("GET /admin/logs/stream", auth(getLogsSSE))
	mux.HandleFunc("POST /admin/core/update", auth(coreUpdate))
	// Graceful self-restart — for settings only read once at boot; see
	// restartService.
	mux.HandleFunc("POST /admin/system/restart", auth(restartService))

	// Lua plugin management
	RegisterLuaAdminRoutes(mux)

	// Egress registry — per-plugin choice of network exit (direct / warp / …)
	RegisterEgressRoutes(mux)

	// Pileus device pairing — rotating code instead of the admin password
	RegisterPairingRoutes(mux)

	// Pileus device management — list / revoke / restore a paired device
	RegisterPileusDeviceRoutes(mux)

	// Pileus profile management ("Utenti") — list / create / rename / delete
	RegisterPileusProfileRoutes(mux)

	// Pileus web-app updater — pull the latest Flutter web build from a
	// configurable repo into data/pileus-web/ (served at /app)
	RegisterPileusWebRoutes(mux)

	// Offline downloads — signed file endpoint + dashboard list/delete
	RegisterDownloadRoutes(mux)
}
