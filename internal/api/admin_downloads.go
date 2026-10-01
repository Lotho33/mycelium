package api

import (
	"net/http"

	"mycelium/internal/downloads"
)

// RegisterDownloadRoutes mounts the public file endpoint (signed URLs minted
// by ListDownloads) and the dashboard's download admin.
func RegisterDownloadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /downloads/file", func(w http.ResponseWriter, r *http.Request) {
		if downloads.M == nil {
			http.Error(w, "download non attivi", http.StatusServiceUnavailable)
			return
		}
		downloads.M.ServeFile(w, r)
	})
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/downloads", auth(listDownloadsAdmin))
	mux.HandleFunc("DELETE /admin/downloads/{id}", auth(deleteDownloadAdmin))
}

func listDownloadsAdmin(w http.ResponseWriter, r *http.Request) {
	if downloads.M == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "downloads": []any{}})
		return
	}
	quota, used, avail := downloads.M.Usage()
	type row struct {
		ID           string  `json:"download_id"`
		Title        string  `json:"title"`
		SeriesTitle  string  `json:"series_title"`
		Season       int32   `json:"season_number"`
		Episode      int32   `json:"episode_number"`
		PluginID     string  `json:"plugin_id"`
		Status       string  `json:"status"`
		Error        string  `json:"error"`
		Progress     float64 `json:"progress"`
		Quality      string  `json:"quality_label"`
		FileBytes    int64   `json:"file_bytes"`
		Estimated    int64   `json:"estimated_bytes"`
		CreatedAt    int64   `json:"created_at"`
		ScheduledFor int64   `json:"scheduled_for"`
		ExpiresAt    int64   `json:"expires_at"`
		Shared       bool    `json:"shared"`
		Upgrade      bool    `json:"upgrade_pending"`
		Fetched      bool    `json:"fetched"`
		Owner        string  `json:"owner_id"`
	}
	rows := []row{}
	for _, in := range downloads.M.ListAll() {
		rows = append(rows, row{in.ID, in.Title, in.SeriesTitle, in.Season, in.Episode, in.PluginID,
			in.Status, in.Error, in.Progress, in.QualityLabel, in.FileBytes, in.EstimatedBytes,
			in.CreatedAt, in.ScheduledFor, in.ExpiresAt, in.Shared, in.UpgradePending, in.Fetched, in.Owner})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":         true,
		"ffmpeg":          downloads.M.FFmpegAvailable(),
		"quota_bytes":     quota,
		"used_bytes":      used,
		"available_bytes": avail,
		"downloads":       rows,
	})
}

func deleteDownloadAdmin(w http.ResponseWriter, r *http.Request) {
	if downloads.M == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "download non attivi"})
		return
	}
	if err := downloads.M.DeleteAdmin(r.PathValue("id")); err != nil {
		code := http.StatusInternalServerError
		if downloads.IsNotFound(err) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
