package api

import (
	"net/http"
	"path/filepath"
	"strings"

	"mycelium/internal/engine"
)

// pluginIcon serves a plugin's branding icon (manifest `icon:`) for the Pileus
// side menu. Public, no auth — an icon is not secret and the client fetches it
// without a token, the same way it hits /pileus/info. 404 when the plugin
// declares no icon or the file is missing; the client then falls back to a
// built-in glyph.
//
// Route: GET /plugin-icon/{plugin_id}
func pluginIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("plugin_id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	path, ok := engine.LuaPlugins.IconPath(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// SVG is the supported format (crisp at any size, themable via
	// currentColor). Anything else is served with a best-effort type but the
	// client only renders SVG.
	switch strings.ToLower(filepath.Ext(path)) {
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	}
	// Icons rarely change; a plugin update writes a new file (fresh mtime) and
	// http.ServeFile's Last-Modified / ETag handling lets the client revalidate.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeFile(w, r, path)
}
