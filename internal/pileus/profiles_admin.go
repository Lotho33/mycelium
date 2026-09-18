// Admin-side profile management (the "Utenti" dashboard tab): list, create,
// rename and delete the server-wide profiles a Pileus client would otherwise
// only manage via its own CreateProfile/ListProfiles/UpdateProfile/
// DeleteProfile gRPC calls. Same pattern as ListDevicesAdmin/RenameDeviceAdmin
// etc. in auth_handler.go — thin wrappers around the same unexported
// insertProfile/updateProfile/deleteProfile helpers, so both paths stay in
// sync with a single source of truth for the schema.
package pileus

import "mycelium/internal/managers"

// AdminProfileInfo is the dashboard view of a profile.
type AdminProfileInfo struct {
	ProfileID string `json:"profile_id"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	CreatedAt int64  `json:"created_at"`
}

// ListProfilesAdmin returns every profile on this server, oldest first.
// Profiles are server-wide (see listProfiles's comment) — there's no
// per-device filtering here either.
func ListProfilesAdmin() ([]AdminProfileInfo, error) {
	rows, err := managers.DB.Query(
		`SELECT profile_id, name, avatar_url, COALESCE(strftime('%s',created_at),'0')
		 FROM pileus_profiles ORDER BY created_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminProfileInfo{}
	for rows.Next() {
		var p AdminProfileInfo
		if err := rows.Scan(&p.ProfileID, &p.Name, &p.AvatarURL, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// adminProfileDeviceID is the audit-only device_id recorded for a profile
// created from the dashboard instead of by a paired device — see
// insertProfile's comment: it's provenance, never looked up as a real device,
// so any readable sentinel is safe here.
const adminProfileDeviceID = "admin-dashboard"

// CreateProfileAdmin creates a profile from the dashboard, independent of any
// device pairing (useful before any Pileus client has ever paired, or just to
// set a profile up ahead of time for someone).
func CreateProfileAdmin(name, avatarURL string) (*AdminProfileInfo, error) {
	profileID := newID()
	if err := insertProfile(adminProfileDeviceID, profileID, name, avatarURL, "{}"); err != nil {
		return nil, err
	}
	return &AdminProfileInfo{ProfileID: profileID, Name: name, AvatarURL: avatarURL}, nil
}

// UpdateProfileAdmin overwrites a profile's name/avatar from the dashboard.
// Unlike the gRPC UpdateProfile (where an empty field means "leave
// unchanged", since a client only sends what it's actually editing), this
// always writes exactly what's passed — the dashboard form round-trips the
// full current state, so there's no ambiguous "unset" case to preserve.
// Preferences are untouched either way; that blob is client-owned.
func UpdateProfileAdmin(profileID, name, avatarURL string) error {
	return updateProfile(profileID, name, avatarURL)
}

// DeleteProfileAdmin removes a profile outright — irreversible. Its watch
// history rows are left orphaned rather than cascaded, same as DeleteDevice
// leaves a device's profiles untouched: no FK ever fires (PRAGMA foreign_keys
// is never turned on on this DB), and watch_history isn't even declared with
// one.
func DeleteProfileAdmin(profileID string) error {
	return deleteProfile(profileID)
}
