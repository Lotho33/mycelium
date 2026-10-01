package downloads

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"mycelium/internal/managers"
)

// Data model: a *blob* is one piece of content on disk — plugin + stream +
// chosen quality/tracks — and the job that produces it; an *item* is one
// profile's download pointing at a blob. Two profiles asking for the same
// episode with the same choices share one blob (one file, one fetch from
// the source); deleting an item only deletes the file when
// no other item still uses it.
const schema = `
CREATE TABLE IF NOT EXISTS dl_blobs (
	blob_id            TEXT PRIMARY KEY,
	dedupe_key         TEXT NOT NULL UNIQUE,     -- plugin \0 stream \0 selection JSON
	plugin_id          TEXT NOT NULL,
	stream_id          TEXT NOT NULL,
	owner_id           TEXT NOT NULL DEFAULT '', -- whose plugin logins resolve it (first requester)
	selection          TEXT NOT NULL DEFAULT '{}',
	status             TEXT NOT NULL,
	error              TEXT NOT NULL DEFAULT '',
	progress           REAL NOT NULL DEFAULT 0,
	estimated_bytes    INTEGER NOT NULL DEFAULT 0,
	file_bytes         INTEGER NOT NULL DEFAULT 0,
	duration_sec       REAL NOT NULL DEFAULT 0,
	quality_label      TEXT NOT NULL DEFAULT '',
	height             INTEGER NOT NULL DEFAULT 0,
	audio_languages    TEXT NOT NULL DEFAULT '',
	subtitle_languages TEXT NOT NULL DEFAULT '',
	use_window         INTEGER NOT NULL DEFAULT 0,
	scheduled_for      INTEGER NOT NULL DEFAULT 0,
	attempts           INTEGER NOT NULL DEFAULT 0,
	created_at         INTEGER NOT NULL,
	started_at         INTEGER NOT NULL DEFAULT 0,
	completed_at       INTEGER NOT NULL DEFAULT 0,
	upgrade            INTEGER NOT NULL DEFAULT 0, -- retry for a better quality
	upgrade_attempts   INTEGER NOT NULL DEFAULT 0,
	upgrade_next       INTEGER NOT NULL DEFAULT 0  -- unix time of the next upgrade check, 0 = none
);
CREATE TABLE IF NOT EXISTS dl_items (
	download_id    TEXT PRIMARY KEY,
	blob_id        TEXT NOT NULL,
	owner_id       TEXT NOT NULL,             -- profile id, or device id without a profile
	device_id      TEXT NOT NULL DEFAULT '',
	media_id       TEXT NOT NULL DEFAULT '',
	parent_id      TEXT NOT NULL DEFAULT '',
	title          TEXT NOT NULL DEFAULT '',
	series_title   TEXT NOT NULL DEFAULT '',
	poster         TEXT NOT NULL DEFAULT '',
	season_number  INTEGER NOT NULL DEFAULT 0,
	episode_number INTEGER NOT NULL DEFAULT 0,
	created_at     INTEGER NOT NULL,
	fetched_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_dl_items_owner ON dl_items(owner_id);
CREATE INDEX IF NOT EXISTS idx_dl_items_blob ON dl_items(blob_id);
-- Best quality seen per plugin and hour of the day (server local time):
-- usual maximum, "below usual" warnings and the learned download window.
CREATE TABLE IF NOT EXISTS dl_quality_hourly (
	plugin_id  TEXT NOT NULL,
	hour       INTEGER NOT NULL,
	max_height INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (plugin_id, hour)
);
-- First draft of this feature (never released): superseded by the tables above.
DROP TABLE IF EXISTS downloads;
DROP TABLE IF EXISTS plugin_quality_stats;`

func ensureSchema() error {
	_, err := managers.DB.Exec(schema)
	return err
}

// ─── blobs ───────────────────────────────────────────────────────────────────

type blob struct {
	ID, Key, PluginID, StreamID, Owner string
	Sel                                Selection
	Status, Error                      string
	Progress                           float64
	EstimatedBytes, FileBytes          int64
	DurationSec                        float64
	QualityLabel                       string
	Height                             int
	AudioLangs, SubLangs               string
	UseWindow                          bool
	ScheduledFor                       int64
	Attempts                           int
	CreatedAt, StartedAt, CompletedAt  int64
	Upgrade                            bool
	UpgradeAttempts                    int
	UpgradeNext                        int64
}

const blobCols = `blob_id, dedupe_key, plugin_id, stream_id, owner_id, selection, status, error, progress,
	estimated_bytes, file_bytes, duration_sec, quality_label, height, audio_languages, subtitle_languages,
	use_window, scheduled_for, attempts, created_at, started_at, completed_at, upgrade, upgrade_attempts, upgrade_next`

func scanBlob(sc interface{ Scan(...any) error }) (*blob, error) {
	var b blob
	var sel string
	var useWindow, upgrade int
	err := sc.Scan(&b.ID, &b.Key, &b.PluginID, &b.StreamID, &b.Owner, &sel, &b.Status, &b.Error, &b.Progress,
		&b.EstimatedBytes, &b.FileBytes, &b.DurationSec, &b.QualityLabel, &b.Height, &b.AudioLangs, &b.SubLangs,
		&useWindow, &b.ScheduledFor, &b.Attempts, &b.CreatedAt, &b.StartedAt, &b.CompletedAt, &upgrade,
		&b.UpgradeAttempts, &b.UpgradeNext)
	if err != nil {
		return nil, err
	}
	b.UseWindow, b.Upgrade = useWindow != 0, upgrade != 0
	_ = json.Unmarshal([]byte(sel), &b.Sel)
	return &b, nil
}

func loadBlob(id string) (*blob, error) {
	b, err := scanBlob(managers.DB.QueryRow(`SELECT `+blobCols+` FROM dl_blobs WHERE blob_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return b, err
}

func blobByKey(key string) (*blob, error) {
	b, err := scanBlob(managers.DB.QueryRow(`SELECT `+blobCols+` FROM dl_blobs WHERE dedupe_key=?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return b, err
}

func blobsWhere(where string, args ...any) []*blob {
	rows, err := managers.DB.Query(`SELECT `+blobCols+` FROM dl_blobs WHERE `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*blob
	for rows.Next() {
		if b, err := scanBlob(rows); err == nil {
			out = append(out, b)
		}
	}
	return out
}

func setBlob(id, set string, args ...any) {
	_, _ = managers.DB.Exec(`UPDATE dl_blobs SET `+set+` WHERE blob_id=?`, append(args, id)...)
}

// ─── items ───────────────────────────────────────────────────────────────────

type item struct {
	ID, BlobID, Owner, Device             string
	MediaID, ParentID, Title, SeriesTitle string
	Poster                                string
	Season, Episode                       int32
	CreatedAt, FetchedAt                  int64
}

const itemCols = `download_id, blob_id, owner_id, device_id, media_id, parent_id, title, series_title,
	poster, season_number, episode_number, created_at, fetched_at`

func scanItem(sc interface{ Scan(...any) error }) (*item, error) {
	var it item
	err := sc.Scan(&it.ID, &it.BlobID, &it.Owner, &it.Device, &it.MediaID, &it.ParentID, &it.Title,
		&it.SeriesTitle, &it.Poster, &it.Season, &it.Episode, &it.CreatedAt, &it.FetchedAt)
	return &it, err
}

// loadItem reads one download; owner "" skips the ownership check.
func loadItem(owner, id string) (*item, error) {
	it, err := scanItem(managers.DB.QueryRow(`SELECT `+itemCols+` FROM dl_items WHERE download_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != "" && it.Owner != owner) {
		return nil, errNotFound
	}
	return it, err
}

func itemsWhere(where string, args ...any) []*item {
	rows, err := managers.DB.Query(`SELECT `+itemCols+` FROM dl_items WHERE `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*item
	for rows.Next() {
		if it, err := scanItem(rows); err == nil {
			out = append(out, it)
		}
	}
	return out
}

func itemCount(blobID string) int {
	var n int
	_ = managers.DB.QueryRow(`SELECT COUNT(*) FROM dl_items WHERE blob_id=?`, blobID).Scan(&n)
	return n
}

// ─── public view ─────────────────────────────────────────────────────────────

// Info is one download as the client sees it: the item plus its blob state.
type Info struct {
	ID, PluginID, StreamID, MediaID, ParentID string
	Title, SeriesTitle, Poster                string
	Season, Episode                           int32
	Status, Error                             string
	Progress                                  float64
	EstimatedBytes, FileBytes                 int64
	QualityLabel                              string
	AudioLangs, SubLangs                      []string
	CreatedAt, ScheduledFor, CompletedAt      int64
	ExpiresAt                                 int64
	FilePath                                  string // signed path+query when completed; the caller prefixes scheme://host
	DurationSec                               float64
	UpgradePending, Shared, Fetched           bool
	Owner                                     string // dashboard only
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func (m *Manager) info(it *item, b *blob) *Info {
	in := &Info{
		ID: it.ID, PluginID: b.PluginID, StreamID: b.StreamID, MediaID: it.MediaID, ParentID: it.ParentID,
		Title: it.Title, SeriesTitle: it.SeriesTitle, Poster: it.Poster, Season: it.Season, Episode: it.Episode,
		Status: b.Status, Error: b.Error, Progress: b.Progress, EstimatedBytes: b.EstimatedBytes,
		FileBytes: b.FileBytes, QualityLabel: b.QualityLabel, AudioLangs: splitList(b.AudioLangs),
		SubLangs: splitList(b.SubLangs), CreatedAt: it.CreatedAt, ScheduledFor: b.ScheduledFor,
		CompletedAt: b.CompletedAt, DurationSec: b.DurationSec,
		UpgradePending: b.Status == StatusCompleted && b.Upgrade && b.UpgradeNext > 0,
		Shared:         itemCount(b.ID) > 1,
		Fetched:        it.FetchedAt > 0,
		Owner:          it.Owner,
	}
	if p, bytes, paused, ok := m.liveState(b.ID); ok {
		in.Progress, in.FileBytes = p, bytes
		if paused {
			in.Status = StatusPaused
		} else if in.Status != StatusCompleted {
			in.Status = StatusRunning
		}
	}
	if b.Status == StatusCompleted {
		if days := retentionDays(); days > 0 && b.CompletedAt > 0 {
			in.ExpiresAt = b.CompletedAt + int64(days)*86400
		}
		if fi, err := os.Stat(m.finalPath(b.ID)); err == nil {
			in.FilePath = signedFilePath(it.ID, fileVersion(fi), m.now().Add(fileURLTTL))
		}
	}
	return in
}

// Get returns one download of owner.
func (m *Manager) Get(owner, id string) (*Info, error) {
	it, err := loadItem(owner, id)
	if err != nil {
		return nil, err
	}
	b, err := loadBlob(it.BlobID)
	if err != nil {
		return nil, err
	}
	return m.info(it, b), nil
}

func (m *Manager) infos(items []*item) []*Info {
	var out []*Info
	for _, it := range items {
		if b, err := loadBlob(it.BlobID); err == nil {
			out = append(out, m.info(it, b))
		}
	}
	return out
}

// List returns owner's downloads, newest first.
func (m *Manager) List(owner string) []*Info {
	return m.infos(itemsWhere(`owner_id=? ORDER BY created_at DESC`, owner))
}

// ListAll returns every download (dashboard), newest first.
func (m *Manager) ListAll() []*Info {
	return m.infos(itemsWhere(`1=1 ORDER BY created_at DESC LIMIT 500`))
}
