// Package api's /admin/* surface — split across files by concern (P2-2):
// admin.go (this file, route table only), admin_session.go (cookie auth),
// admin_auth_pages.go (login/logout/dashboard HTML), admin_log.go (in-memory
// log buffer + SSE stream), admin_clients.go (hub API client management),
// admin_wipe.go (destructive reset actions), admin_system.go (settings/
// status/info/self-update), admin_password.go (admin password change). One
// 1000+-line file with every concern mixed in used to make this harder to
// navigate than it needed to be — the split is purely a code move, no
// behaviour or package boundary changed.
package api

import "net/http"

func AdminRoutes(mux *http.ServeMux) {
	// Login/logout — pubblici (rate limited sul POST)
	mux.HandleFunc("GET /admin/login", adminLoginPage)
	mux.HandleFunc("POST /admin/login", rateLimitMiddleware(loginLimiter, adminLoginSubmit))
	mux.HandleFunc("POST /admin/logout", adminLogout)

	// PWA (manifest + service worker) — pubblici apposta: la pagina di login
	// deve poter essere installata anche prima di autenticarsi, esattamente
	// come qualunque altra PWA con un flusso di accesso. Vedi admin_pwa.go.
	mux.HandleFunc("GET /admin/manifest.json", serveAdminManifest)
	mux.HandleFunc("GET /admin/sw.js", serveAdminServiceWorker)

	// Tutto il resto richiede sessione valida
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/dashboard", auth(adminDashboard))
	mux.HandleFunc("GET /admin/logs", auth(getLogs))

	mux.HandleFunc("GET /admin/clients", auth(listClients))
	mux.HandleFunc("POST /admin/clients", auth(addClient))
	mux.HandleFunc("DELETE /admin/clients/{client_id}", auth(removeClient))
	mux.HandleFunc("GET /admin/dev-token", auth(generateDevToken))

	// Lua plugin management lives under /admin/lua-plugins/* (RegisterLuaAdminRoutes).

	mux.HandleFunc("POST /admin/settings/save", auth(saveSettings))
	// Rate limited like the wipes below: it verifies the current password,
	// so a stolen cookie could otherwise brute-force it without limit.
	mux.HandleFunc("POST /admin/password/change", rateLimitMiddleware(wipeLimiter, auth(changeAdminPasswordHandler)))
	mux.HandleFunc("POST /admin/cache/clear", auth(clearCacheHandler))
	// Rate limited (wipeLimiter, same 5/min budget as /admin/login): both
	// re-check the admin password in the body, so a stolen session cookie
	// without the password could otherwise be brute-forced with no limit.
	mux.HandleFunc("POST /admin/profiles/wipe", rateLimitMiddleware(wipeLimiter, auth(wipeProfilesHandler)))
	mux.HandleFunc("POST /admin/plugins/wipe-data", rateLimitMiddleware(wipeLimiter, auth(wipePluginDataHandler)))
	mux.HandleFunc("POST /admin/factory-reset", rateLimitMiddleware(wipeLimiter, auth(factoryResetHandler)))
	mux.HandleFunc("GET /admin/core/resources", auth(getCoreResources))
	mux.HandleFunc("GET /admin/plugins/info", auth(getPluginsInfo))
	mux.HandleFunc("GET /admin/enricher-bindings", auth(getEnricherBindings))
	mux.HandleFunc("GET /admin/info", auth(getAdminInfo))
	mux.HandleFunc("GET /admin/system/status", auth(getSystemStatus))
	// Generic proxy URL for plugin egress (VPN/WARP/whatever the operator
	// wires up). Not surfaced in the dashboard for now — VPN_PROXY_URL in the
	// environment is the primary knob; a per-operator config UI comes later.
	mux.HandleFunc("POST /admin/vpn", auth(setVPNProxy))
	mux.HandleFunc("POST /admin/vpn/test", auth(testVPNConnection))
	mux.HandleFunc("GET /admin/logs/stream", auth(getLogsSSE))
	mux.HandleFunc("POST /admin/core/update", auth(coreUpdate))

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
}
