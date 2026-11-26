package managers

import (
	"database/sql"
	"strconv"
	"time"
)

// series_latest: the latest episode out of each series someone follows, as
// the plugin's details said on the last check, and since when the server
// knows it (found_at). Shared by every profile: it is a fact about the
// series. Each profile's "new episode" is worked out at read time from it
// and the profile's own history (pileus/new_episodes.go).
const SeriesLatestSchema = `
CREATE TABLE IF NOT EXISTS series_latest (
	plugin_id      TEXT NOT NULL,
	series_id      TEXT NOT NULL,
	season_number  INTEGER NOT NULL DEFAULT 0,
	episode_number INTEGER NOT NULL DEFAULT 0,
	air_date       TEXT NOT NULL DEFAULT '',
	found_at       INTEGER NOT NULL DEFAULT 0,
	checked_at     INTEGER NOT NULL DEFAULT 0,
	card           BLOB,
	PRIMARY KEY (plugin_id, series_id)
);`

// SeriesLatest is one series_latest row. FoundAt is 0 when the episode was
// already out the first time the series was checked (not "new" to anyone).
// Card is the series' card as of the last check (a marshalled CatalogItem:
// backdrop, plot, genres, logo), so a "new episode" row looks like the other
// rows without asking the plugin again; empty when the details failed, nil
// when the row predates the column.
type SeriesLatest struct {
	PluginID, SeriesID string
	Season, Episode    int32
	AirDate            string
	FoundAt, CheckedAt int64
	Card               []byte
	HasCard            bool
}

// EpisodeAfter reports whether s/e comes after season/episode.
func EpisodeAfter(s, e, season, episode int32) bool {
	return s > season || (s == season && e > episode)
}

// SetSeriesLatest records a check of a series. A later episode than the
// stored one gets found_at = now; the first check of a series stores found_at
// = 0 (whatever is out then isn't news); season/episode 0 = only the check
// time moves (the plugin said nothing). newer: a later episode than stored.
func (m *DBManager) SetSeriesLatest(pluginID, seriesID string, season, episode int32, airDate string) (newer bool, err error) {
	now := time.Now().Unix()
	if episode <= 0 {
		_, err = m.db.Exec(`INSERT INTO series_latest(plugin_id, series_id, checked_at) VALUES(?,?,?)
			ON CONFLICT(plugin_id, series_id) DO UPDATE SET checked_at = excluded.checked_at`,
			pluginID, seriesID, now)
		return false, err
	}
	var cs, ce int32
	err = m.db.QueryRow(`SELECT season_number, episode_number FROM series_latest
		WHERE plugin_id = ? AND series_id = ?`, pluginID, seriesID).Scan(&cs, &ce)
	switch {
	case err == sql.ErrNoRows || (err == nil && ce == 0):
		_, err = m.db.Exec(`INSERT INTO series_latest(plugin_id, series_id, season_number,
				episode_number, air_date, found_at, checked_at) VALUES(?,?,?,?,?,0,?)
			ON CONFLICT(plugin_id, series_id) DO UPDATE SET season_number = excluded.season_number,
				episode_number = excluded.episode_number, air_date = excluded.air_date,
				found_at = 0, checked_at = excluded.checked_at`,
			pluginID, seriesID, season, episode, airDate, now)
	case err != nil:
		return false, err
	case EpisodeAfter(season, episode, cs, ce):
		newer = true
		_, err = m.db.Exec(`UPDATE series_latest SET season_number = ?, episode_number = ?,
				air_date = ?, found_at = ?, checked_at = ?
			WHERE plugin_id = ? AND series_id = ?`,
			season, episode, airDate, now, now, pluginID, seriesID)
	default:
		// Same episode (or an earlier one: the source lost it for a while):
		// keep what is stored, only the check time moves.
		_, err = m.db.Exec(`UPDATE series_latest SET checked_at = ? WHERE plugin_id = ? AND series_id = ?`,
			now, pluginID, seriesID)
	}
	return newer && err == nil, err
}

// SetSeriesCard stores the series' card from the last check (empty = the
// details didn't come: tried, nothing to show).
func (m *DBManager) SetSeriesCard(pluginID, seriesID string, card []byte) error {
	if card == nil {
		card = []byte{}
	}
	_, err := m.db.Exec(`UPDATE series_latest SET card = ? WHERE plugin_id = ? AND series_id = ?`,
		card, pluginID, seriesID)
	return err
}

// AllSeriesLatest returns every known latest episode, keyed by plugin and
// series id.
func (m *DBManager) AllSeriesLatest() (map[[2]string]SeriesLatest, error) {
	rows, err := m.db.Query(`SELECT plugin_id, series_id, season_number, episode_number,
		air_date, found_at, checked_at, card FROM series_latest`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]SeriesLatest{}
	for rows.Next() {
		var s SeriesLatest
		var card []byte
		if err := rows.Scan(&s.PluginID, &s.SeriesID, &s.Season, &s.Episode, &s.AirDate,
			&s.FoundAt, &s.CheckedAt, &card); err != nil {
			return nil, err
		}
		s.Card, s.HasCard = card, card != nil
		out[[2]string{s.PluginID, s.SeriesID}] = s
	}
	return out, rows.Err()
}

// FollowedSeries is every series some profile follows: in a list as a
// series, or watched (an episode row with its series id) in the last
// activeDays. Most recently active first. checkedBefore skips the series
// checked since then.
func (m *DBManager) FollowedSeries(activeDays int, checkedBefore int64) ([][2]string, error) {
	rows, err := m.db.Query(`
		WITH followed(plugin_id, series_id, active) AS (
			SELECT plugin_id, media_id, added_at FROM my_list WHERE media_type = 'series'
			UNION ALL
			SELECT provider_id, parent_id, CAST(strftime('%s', last_updated) AS INTEGER)
			FROM watch_history
			WHERE parent_id != '' AND last_updated >= datetime('now', ?)
		)
		SELECT f.plugin_id, f.series_id, MAX(f.active) AS act FROM followed f
		LEFT JOIN series_latest l ON l.plugin_id = f.plugin_id AND l.series_id = f.series_id
		-- card IS NULL: checked before cards were kept, fetch it once now.
		WHERE COALESCE(l.checked_at, 0) < ? OR (l.series_id IS NOT NULL AND l.card IS NULL)
		GROUP BY f.plugin_id, f.series_id
		ORDER BY act DESC`,
		"-"+strconv.Itoa(max(activeDays, 1))+" days", checkedBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var p, s string
		var act sql.NullInt64
		if err := rows.Scan(&p, &s, &act); err != nil {
			return nil, err
		}
		out = append(out, [2]string{p, s})
	}
	return out, rows.Err()
}

// SeriesWatch is where an owner got to in a series: the episode of its
// watch_history row, whether it was finished, and when it was last watched.
type SeriesWatch struct {
	Season, Episode int32
	Completed       bool
	LastWatched     int64
	Title, Poster   string
}

// SeriesWatchState returns owner's position in each series it watched.
func (m *DBManager) SeriesWatchState(owner string) (map[[2]string]SeriesWatch, error) {
	rows, err := m.db.Query(`SELECT provider_id, parent_id, season_number, episode_number,
			is_completed, CAST(strftime('%s', last_updated) AS INTEGER),
			navigation_context, poster
		FROM watch_history WHERE client_id = ? AND parent_id != ''`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]string]SeriesWatch{}
	for rows.Next() {
		var p, s string
		var w SeriesWatch
		var last sql.NullInt64
		var title, poster sql.NullString
		if err := rows.Scan(&p, &s, &w.Season, &w.Episode, &w.Completed, &last, &title, &poster); err != nil {
			return nil, err
		}
		w.LastWatched, w.Title, w.Poster = last.Int64, title.String, poster.String
		k := [2]string{p, s}
		// One row per series is the rule (UpsertProgress); keep the furthest
		// should an old database hold more.
		if cur, ok := out[k]; !ok || EpisodeAfter(w.Season, w.Episode, cur.Season, cur.Episode) {
			out[k] = w
		}
	}
	return out, rows.Err()
}
