// Admin-side Pileus device management: list paired devices, revoke / restore a
// device's access, or forget it outright. Revoking sets
// pileus_devices.revoked_at so the gRPC auth interceptor (internal/pileus)
// rejects every JWT that device holds without touching its profiles; delete
// removes the device row entirely (still without touching profiles — see
// pileus.DeleteDevice) for when revoke alone isn't enough (stolen device,
// decommissioned TV, tidying up the list). Same auth boundary as /admin/*.
package api

import (
	"encoding/json"
	"net/http"

	"mycelium/internal/pileus"
)

// RegisterPileusDeviceRoutes wires the device-management endpoints onto mux.
func RegisterPileusDeviceRoutes(mux *http.ServeMux) {
	auth := adminAuthMiddleware
	mux.HandleFunc("GET /admin/pileus/devices", auth(listPileusDevices))
	mux.HandleFunc("POST /admin/pileus/devices/{id}/rename", auth(renamePileusDevice))
	mux.HandleFunc("POST /admin/pileus/devices/{id}/revoke", auth(revokePileusDevice))
	mux.HandleFunc("POST /admin/pileus/devices/{id}/unrevoke", auth(unrevokePileusDevice))
	mux.HandleFunc("DELETE /admin/pileus/devices/{id}", auth(deletePileusDevice))
}

// GET /admin/pileus/devices → {devices: [{device_id,label,created_at,last_seen_at,revoked}]}
func listPileusDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := pileus.ListDevicesAdmin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devs})
}

// POST /admin/pileus/devices/{id}/rename  body: {"label": "TV salotto"}
// Same effect as the gRPC RenameDevice a Pileus client calls on itself, but
// from the dashboard — for giving a device a human name instead of everyone
// having to recognise it by its opaque device_id.
func renamePileusDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "device id required"})
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "invalid JSON body"})
		return
	}
	stored, err := pileus.RenameDeviceAdmin(id, body.Label)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device_id": id, "label": stored})
}

// POST /admin/pileus/devices/{id}/revoke
func revokePileusDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "device id required"})
		return
	}
	if err := pileus.RevokeDevice(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
}

// POST /admin/pileus/devices/{id}/unrevoke
func unrevokePileusDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "device id required"})
		return
	}
	if err := pileus.UnrevokeDevice(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unrevoked": id})
}

// DELETE /admin/pileus/devices/{id} — forgets the device outright (harder than
// revoke: it disappears from the list). Profiles are untouched, see DeleteDevice.
func deletePileusDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "device id required"})
		return
	}
	if err := pileus.DeleteDevice(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
