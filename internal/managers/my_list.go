package managers

import (
	"encoding/json"
	"time"
)

// MyListSchema is the "La mia lista" table: titles a profile saved to watch
// later. owner_id is the profile id (the device id for a client with no
// profile, same rule as watch_history's client_id). The card fields are
// stored as the client sent them — the plugin is never asked again.
const MyListSchema = `
CREATE TABLE IF NOT EXISTS my_list (
	owner_id   TEXT NOT NULL,
	plugin_id  TEXT NOT NULL,
	media_id   TEXT NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	poster_url TEXT NOT NULL DEFAULT '',
	media_type TEXT NOT NULL DEFAULT '',
	year       INTEGER NOT NULL DEFAULT 0,
	rating     REAL NOT NULL DEFAULT 0,
	fanart_url TEXT NOT NULL DEFAULT '',
	logo_url   TEXT NOT NULL DEFAULT '',
	added_at   INTEGER NOT NULL,
	extra_json TEXT NOT NULL DEFAULT '{}',
	PRIMARY KEY (owner_id, plugin_id, media_id)
);`

const (
	// MyListMax caps one owner's list: past it the oldest entries go, so a
	// forgotten list can't grow without bound.
	MyListMax = 500
	// myListMaxExtra bounds one card's metadata; a bigger one is not stored.
	myListMaxExtra = 32 << 10
)

// MyListEntry is one my_list row.
type MyListEntry struct {
	PluginID, MediaID, Title, PosterURL, MediaType string
	Year                                           int32
	Rating                                         float64
	FanartURL, LogoURL                             string
	AddedAt                                        int64
	Extra                                          map[string]string
}

// AddToMyList inserts e for owner, or refreshes its card if it is already
// there (keeping its place: added_at doesn't move). A blank field never
// overwrites a stored one: a client that only has part of the card must not
// wipe the rest.
func (m *DBManager) AddToMyList(owner string, e MyListEntry) error {
	extra := "{}"
	if len(e.Extra) > 0 {
		if b, err := json.Marshal(e.Extra); err == nil && len(b) <= myListMaxExtra {
			extra = string(b)
		}
	}
	_, err := m.db.Exec(`INSERT INTO my_list(owner_id, plugin_id, media_id, title,
			poster_url, media_type, year, rating, fanart_url, logo_url, added_at,
			extra_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(owner_id, plugin_id, media_id) DO UPDATE SET
			title=COALESCE(NULLIF(excluded.title,''), title),
			poster_url=COALESCE(NULLIF(excluded.poster_url,''), poster_url),
			media_type=COALESCE(NULLIF(excluded.media_type,''), media_type),
			year=CASE WHEN excluded.year>0 THEN excluded.year ELSE year END,
			rating=CASE WHEN excluded.rating>0 THEN excluded.rating ELSE rating END,
			fanart_url=COALESCE(NULLIF(excluded.fanart_url,''), fanart_url),
			logo_url=COALESCE(NULLIF(excluded.logo_url,''), logo_url),
			extra_json=CASE WHEN excluded.extra_json<>'{}' THEN excluded.extra_json ELSE extra_json END`,
		owner, e.PluginID, e.MediaID, e.Title, e.PosterURL, e.MediaType,
		e.Year, e.Rating, e.FanartURL, e.LogoURL, time.Now().Unix(), extra)
	if err != nil {
		return err
	}
	_, err = m.db.Exec(`DELETE FROM my_list WHERE owner_id = ? AND rowid NOT IN (
		SELECT rowid FROM my_list WHERE owner_id = ?
		ORDER BY added_at DESC, rowid DESC LIMIT ?)`, owner, owner, MyListMax)
	return err
}

// RemoveFromMyList deletes one entry; a missing one is not an error.
func (m *DBManager) RemoveFromMyList(owner, pluginID, mediaID string) error {
	_, err := m.db.Exec(
		`DELETE FROM my_list WHERE owner_id = ? AND plugin_id = ? AND media_id = ?`,
		owner, pluginID, mediaID)
	return err
}

// GetMyList returns owner's list, most recently added first; pluginID
// filters it when not empty.
func (m *DBManager) GetMyList(owner, pluginID string) ([]MyListEntry, error) {
	q := `SELECT plugin_id, media_id, title, poster_url, media_type, year, rating,
		fanart_url, logo_url, added_at, extra_json FROM my_list WHERE owner_id = ?`
	args := []any{owner}
	if pluginID != "" {
		q += ` AND plugin_id = ?`
		args = append(args, pluginID)
	}
	rows, err := m.db.Query(q+` ORDER BY added_at DESC, rowid DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MyListEntry
	for rows.Next() {
		var e MyListEntry
		var extra string
		if err := rows.Scan(&e.PluginID, &e.MediaID, &e.Title, &e.PosterURL,
			&e.MediaType, &e.Year, &e.Rating, &e.FanartURL, &e.LogoURL,
			&e.AddedAt, &extra); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(extra), &e.Extra) // a bad blob just shows no metadata
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteMyListForOwner removes a whole list (profile deleted).
func (m *DBManager) DeleteMyListForOwner(owner string) error {
	_, err := m.db.Exec(`DELETE FROM my_list WHERE owner_id = ?`, owner)
	return err
}
