// Admin dashboard PWA installability — manifest + service worker for
// /admin/login and /admin/dashboard, mirroring the same install-banner
// criteria the Pileus web app already satisfies under /app/ (see setup.go's
// serveManifest / web/static/manifest.json). Kept as a separate manifest/SW
// (not a reuse of the Pileus one) because that one's start_url/scope are
// locked to /app/ — installing "Mycelium Admin" must land on
// /admin/dashboard, not the Pileus player.
package api

import (
	"net/http"
	"path/filepath"

	"mycelium/internal/core"
)

// serveAdminManifest serves web/static/admin-manifest.json at
// /admin/manifest.json (not /static/admin-manifest.json) purely so the URL
// reads naturally next to the other /admin/* pages that link it; the file
// itself lives in web/static/ alongside every other static asset.
func serveAdminManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	http.ServeFile(w, r, filepath.Join(core.BasePath, "web", "static", "admin-manifest.json"))
}

// serveAdminServiceWorker serves web/static/admin-sw.js at /admin/sw.js.
// The path matters, not just the content: a service worker's maximum
// controllable scope defaults to the directory of its own script URL, and
// the admin manifest/registration ask for scope "/admin/" — serving the
// exact same file bytes from /static/admin-sw.js instead would cap it to
// scope "/static/", missing every /admin/* page entirely (short of also
// sending a Service-Worker-Allowed response header, which this sidesteps).
func serveAdminServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	// Never cache the worker script itself — the browser already has its own
	// (spec-mandated) revalidation logic for service workers, and the blanket
	// "Cache-Control: no-store" main.go sets on /static/* doesn't apply here
	// since this route isn't served through that file server.
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(core.BasePath, "web", "static", "admin-sw.js"))
}
