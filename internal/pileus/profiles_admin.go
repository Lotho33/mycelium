// Dashboard profile management: list, create, rename and delete the
// server-wide profiles, through the same helpers as the gRPC calls.
package pileus

import "mycelium/internal/managers"

// AdminProfileInfo is the dashboard view of a profile.
type AdminProfileInfo struct {
	ProfileID string `json:"profile_id"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	CreatedAt int64  `json:"created_at"`
	// Profile PIN: whether one is set, and the devices trusted for it.
	PinProtected   bool                `json:"pin_protected"`
	TrustedDevices []TrustedDeviceInfo `json:"trusted_devices"`
}

// ListProfilesAdmin returns every profile on this server, oldest first.
func ListProfilesAdmin() ([]AdminProfileInfo, error) {
	rows, err := managers.DB.Query(
		`SELECT profile_id, name, avatar_url, COALESCE(strftime('%s',created_at),'0'),
		        COALESCE(access_pin_hash,'') != ''
		 FROM pileus_profiles ORDER BY created_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminProfileInfo{}
	for rows.Next() {
		var p AdminProfileInfo
		if err := rows.Scan(&p.ProfileID, &p.Name, &p.AvatarURL, &p.CreatedAt, &p.PinProtected); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		t, err := listTrustedDevices(out[i].ProfileID)
		if err != nil {
			return nil, err
		}
		out[i].TrustedDevices = t
	}
	return out, nil
}

// adminProfileDeviceID is the audit-only device_id of profiles created from
// the dashboard.
const adminProfileDeviceID = "admin-dashboard"

// CreateProfileAdmin creates a profile from the dashboard.
func CreateProfileAdmin(name, avatarURL string) (*AdminProfileInfo, error) {
	profileID := newID()
	if err := insertProfile(adminProfileDeviceID, profileID, name, avatarURL, "{}"); err != nil {
		return nil, err
	}
	return &AdminProfileInfo{ProfileID: profileID, Name: name, AvatarURL: avatarURL}, nil
}

// UpdateProfileAdmin writes a profile's name/avatar exactly as given (the
// form sends the full state). Preferences are untouched.
func UpdateProfileAdmin(profileID, name, avatarURL string) error {
	return updateProfile(profileID, name, avatarURL)
}

// DeleteProfileAdmin removes a profile. Its watch-history rows are left as
// they are.
func DeleteProfileAdmin(profileID string) error {
	return deleteProfile(profileID)
}
