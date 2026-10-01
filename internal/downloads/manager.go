// Package downloads prepares offline downloads on the server: it resolves a
// stream through the plugin, fetches every segment of the chosen HLS
// renditions through mycelium's own /proxy (so the plugin's egress, headers,
// AES-128 keys and token recovery apply) into a work directory, then lets
// ffmpeg mux those local files (no re-encoding) into one Matroska file.
// Devices fetch the file over a signed URL (http.go).
//
// Fetching segment by segment makes a job resumable and pausable. Segments
// are deleted as soon as ffmpeg has consumed them, so disk usage stays close
// to the file size; every job is checked against a quota and a free-space
// margin, finished files expire, identical downloads share one file, and a
// file can be dropped once the device confirmed it has it.
package downloads

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// Status values (also the wire values of DownloadInfo.status).
const (
	StatusQueued    = "queued"
	StatusScheduled = "scheduled"
	StatusRunning   = "running"
	StatusPaused    = "paused"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

const (
	maxAttempts        = 6
	maxUpgradeAttempts = 3
	optionsTTL         = 10 * time.Minute
	schedulerTick      = 15 * time.Second
	cleanupTick        = time.Hour
	streamingIdle      = 45 * time.Second // a player fetched something this recently = someone is watching
)

// Source is a resolved stream as the download worker consumes it.
type Source struct {
	URL     string            // playlist URL (a loopback /proxy URL, or upstream for direct_stream plugins)
	Headers map[string]string // extra request headers, only for non-proxied sources
	IsHLS   bool
	IsLive  bool
	Extra   map[string]string
}

// Resolver resolves pluginID/streamID for a download on behalf of owner (the
// profile, or device, whose plugin logins apply). Set by the pileus package
// at startup: it owns the plugin pipeline and the /proxy URL minting.
type Resolver func(ctx context.Context, pluginID, streamID, owner string) (Source, error)

// ErrUnavailable wraps a user-facing "can't download this" reason.
type ErrUnavailable struct{ Reason string }

func (e ErrUnavailable) Error() string { return e.Reason }

var errNotFound = errors.New("download non trovato")

// IsNotFound reports whether err means the download doesn't exist (for owner).
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }

// ─── hooks (vars so tests can stand in) ──────────────────────────────────────

// pluginDownloadConfig reads a plugin's manifest download section.
var pluginDownloadConfig = func(pluginID string) engine.LuaManifestDownload {
	return engine.LuaPlugins.DownloadConfig(pluginID)
}

// setting reads a server setting.
var setting = func(key, def string) string { return managers.Settings.GetString(key, def) }

// streamingActive reports whether some player is watching something now
// (it fetched through /proxy within streamingIdle).
var streamingActive = func() bool { return managers.Sessions.ActiveWithin(streamingIdle) }

// ─── settings ────────────────────────────────────────────────────────────────

func settingFloat(key string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(setting(key, "")), 64)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// Defaults sized for small disks: at most 20 GB of downloads, never below
// 3 GB of free space.
func quotaBytes() int64         { return int64(settingFloat("download_quota_gb", 20) * 1e9) }
func minFreeBytes() int64       { return int64(settingFloat("download_min_free_gb", 3) * 1e9) }
func retentionDays() int        { return int(settingFloat("download_retention_days", 7)) }
func concurrency() int          { return max(1, int(settingFloat("download_concurrency", 1))) }
func enabledGlobally() bool     { return setting("download_enabled", "1") != "0" }
func deleteAfterFetch() bool    { return setting("download_delete_after_fetch", "0") == "1" }
func pauseWhileStreaming() bool { return setting("download_pause_while_streaming", "1") != "0" }
func maxBytesPerSec() int64     { return int64(settingFloat("download_max_mbps", 0) * 125_000) }

// paceFactor caps how many times faster than real time segments are fetched
// (0 = no cap): some sources throttle a whole episode pulled in one burst.
func paceFactor() float64 { return settingFloat("download_pace", 4) }

// ─── manager ─────────────────────────────────────────────────────────────────

type liveJob struct {
	cancel   context.CancelFunc
	upgrade  bool // background quality upgrade: invisible to the client
	progress float64
	bytes    int64
	paused   bool
}

type cachedOptions struct {
	opts Options
	at   time.Time
}

// Manager is the process-wide download service (see M).
type Manager struct {
	dir     string
	resolve Resolver
	ffmpeg  string
	http    *http.Client
	now     func() time.Time

	mu       sync.Mutex
	live     map[string]*liveJob // blobID ->
	options  map[string]cachedOptions
	kick     chan struct{}
	createMu sync.Mutex

	// runJob is the job body; a var so tests can stub the fetch+mux out.
	runJob func(ctx context.Context, b *blob, upgrade bool) error
}

// M is nil until Init.
var M *Manager

// Init creates the download service: tables, directory, crash recovery.
// ffmpeg is looked up on PATH (MYCELIUM_FFMPEG overrides).
func Init(dir string, resolve Resolver) (*Manager, error) {
	if err := ensureSchema(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ff := os.Getenv("MYCELIUM_FFMPEG")
	if ff == "" {
		ff, _ = exec.LookPath("ffmpeg")
	}
	m := &Manager{
		dir:     dir,
		resolve: resolve,
		ffmpeg:  ff,
		http:    &http.Client{Timeout: 90 * time.Second},
		now:     time.Now,
		live:    map[string]*liveJob{},
		options: map[string]cachedOptions{},
		kick:    make(chan struct{}, 1),
	}
	m.runJob = m.runBlob
	m.recoverAfterRestart()
	return m, nil
}

// Start runs the scheduler and the cleanup until ctx ends.
func (m *Manager) Start(ctx context.Context) {
	core.SafeGo("downloads/scheduler", func() { m.schedulerLoop(ctx) })
	core.SafeGo("downloads/cleanup", func() { m.cleanupLoop(ctx) })
}

// FFmpegAvailable reports whether downloads can run at all.
func (m *Manager) FFmpegAvailable() bool { return m.ffmpeg != "" }

func (m *Manager) wake() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// ─── paths ───────────────────────────────────────────────────────────────────

func (m *Manager) finalPath(id string) string  { return filepath.Join(m.dir, id+".mkv") }
func (m *Manager) partPath(id string) string   { return filepath.Join(m.dir, id+".mkv.part") }
func (m *Manager) workDir(id string) string    { return filepath.Join(m.dir, id+".work") }
func (m *Manager) upPartPath(id string) string { return filepath.Join(m.dir, id+".up.mkv.part") }
func (m *Manager) upWorkDir(id string) string  { return filepath.Join(m.dir, id+".up.work") }

func (m *Manager) removeBlobFiles(id string) {
	_ = os.Remove(m.finalPath(id))
	_ = os.Remove(m.partPath(id))
	_ = os.RemoveAll(m.workDir(id))
	_ = os.Remove(m.upPartPath(id))
	_ = os.RemoveAll(m.upWorkDir(id))
}

// ─── space ───────────────────────────────────────────────────────────────────

// usedBytes is everything under the downloads dir: finished files, work
// directories of running/paused jobs, partial outputs.
func (m *Manager) usedBytes() int64 {
	var total int64
	_ = filepath.WalkDir(m.dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

// AvailableBytes is how much downloads may still use: the smaller of the
// quota headroom and the disk's free space above the safety margin.
func (m *Manager) AvailableBytes() int64 {
	avail := quotaBytes() - m.usedBytes()
	if free, ok := diskFree(m.dir); ok {
		if byDisk := free - minFreeBytes(); byDisk < avail {
			avail = byDisk
		}
	}
	return max(0, avail)
}

// Usage returns (quota, used, available) for the dashboard / ListDownloads.
func (m *Manager) Usage() (quota, used, available int64) {
	return quotaBytes(), m.usedBytes(), m.AvailableBytes()
}

func spaceError(need, avail int64) error {
	return ErrUnavailable{Reason: fmt.Sprintf(
		"spazio insufficiente sul server: servono circa %s, disponibili %s (quota e margine di sicurezza del disco nelle impostazioni)",
		humanBytes(need), humanBytes(avail))}
}

func humanBytes(b int64) string {
	switch {
	case b >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(b)/1e9)
	case b >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(b)/1e6)
	}
	return fmt.Sprintf("%d KB", b/1000)
}

// ─── options ─────────────────────────────────────────────────────────────────

// Options is everything GetDownloadOptions reports.
type Options struct {
	Available            bool
	Reason               string
	DurationSec          float64
	Variants             []OptVariant
	Audio                []OptTrack
	Subtitles            []OptTrack
	PluginNote           string
	PluginNotice         string
	PreferredHours       string
	PreferredHoursSource string
	QualityBelowUsual    bool
	UsualMaxHeight       int
	FreeBytes            int64
	BytesPerSec          int64
	RetentionDays        int
}

// OptVariant / OptTrack are the offered choices.
type OptVariant struct {
	ID             string
	Width, Height  int
	Bandwidth      int64
	Peak           bool
	Codecs, Label  string
	EstimatedBytes int64
}

type OptTrack struct {
	ID, Language, Name string
	Default            bool
	EstimatedBytes     int64
}

func unavailable(reason string) Options { return Options{Reason: reason} }

// fetchText GETs a playlist (loopback /proxy or, for direct plugins, upstream).
func (m *Manager) fetchText(ctx context.Context, u string, headers map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("playlist: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(b), err
}

// probe resolves and parses a stream: master, the media playlist duration.
// Every probe also teaches the per-hour quality statistics.
func (m *Manager) probe(ctx context.Context, pluginID, streamID, owner string) (Source, Master, float64, error) {
	src, err := m.resolve(ctx, pluginID, streamID, owner)
	if err != nil {
		return src, Master{}, 0, err
	}
	if src.IsLive {
		return src, Master{}, 0, ErrUnavailable{"Le dirette non si possono scaricare"}
	}
	if !src.IsHLS {
		return src, Master{}, 0, ErrUnavailable{"Questa sorgente non è scaricabile (è supportato solo lo streaming HLS)"}
	}
	text, err := m.fetchText(ctx, src.URL, src.Headers)
	if err != nil {
		return src, Master{}, 0, err
	}
	master := ParsePlaylist(text, src.URL)
	if len(master.Variants) == 0 {
		return src, master, 0, ErrUnavailable{"Playlist senza flussi video"}
	}
	media := text
	if master.IsMaster {
		if media, err = m.fetchText(ctx, master.Variants[0].URI, src.Headers); err != nil {
			return src, master, 0, err
		}
	}
	dur, ended := MediaDuration(media)
	if !ended {
		return src, master, 0, ErrUnavailable{"Le dirette non si possono scaricare"}
	}
	recordQuality(pluginID, master.Variants[0].Height, m.now())
	return src, master, dur, nil
}

// Options computes the download choices for a stream.
func (m *Manager) Options(ctx context.Context, pluginID, streamID, owner string) (Options, error) {
	cfg := pluginDownloadConfig(pluginID)
	if !cfg.Enabled {
		return unavailable("Questo plugin non permette il download"), nil
	}
	if !enabledGlobally() {
		return unavailable("I download sono disattivati sul server"), nil
	}
	if !m.FFmpegAvailable() {
		return unavailable("ffmpeg non è disponibile sul server"), nil
	}
	now := m.now()
	usual := usualMaxHeight(pluginID, now) // before this probe updates it
	src, master, dur, err := m.probe(ctx, pluginID, streamID, owner)
	var ua ErrUnavailable
	if errors.As(err, &ua) {
		return unavailable(ua.Reason), nil
	}
	if err != nil {
		return Options{}, err
	}

	o := Options{
		Available:     true,
		DurationSec:   dur,
		PluginNote:    cfg.Note,
		FreeBytes:     m.AvailableBytes(),
		BytesPerSec:   recentBytesPerSec(),
		RetentionDays: retentionDays(),
	}
	o.PreferredHours, o.PreferredHoursSource = preferredWindow(pluginID, now)
	if src.Extra != nil {
		o.PluginNotice = src.Extra["download_notice"]
	}
	for _, v := range master.Variants {
		o.Variants = append(o.Variants, OptVariant{
			ID: variantID(v), Width: v.Width, Height: v.Height, Bandwidth: v.Bandwidth,
			Peak: v.Peak, Codecs: v.Codecs, Label: variantLabel(v),
			EstimatedBytes: estimateBytes(v.Bandwidth, dur),
		})
	}
	seen := map[string]bool{}
	for _, r := range master.Renditions {
		id := trackID(r)
		if seen[id] || (r.Type == "AUDIO" && r.URI == "") {
			continue // audio inside the variant: nothing to choose
		}
		seen[id] = true
		t := OptTrack{ID: id, Language: r.Language, Name: r.Name, Default: r.Default}
		if r.Type == "AUDIO" {
			t.EstimatedBytes = estimateBytes(assumedAudioBitrate, dur)
			o.Audio = append(o.Audio, t)
		} else {
			o.Subtitles = append(o.Subtitles, t)
		}
	}
	best := master.Variants[0].Height
	o.UsualMaxHeight = max(usual, best)
	o.QualityBelowUsual = best > 0 && usual > best

	m.mu.Lock()
	for k, c := range m.options { // entries past optionsTTL are never read again
		if now.Sub(c.at) > optionsTTL {
			delete(m.options, k)
		}
	}
	m.options[owner+"\x00"+pluginID+"\x00"+streamID] = cachedOptions{opts: o, at: now}
	m.mu.Unlock()
	return o, nil
}

// cachedOrFreshOptions reuses what the client was just shown.
func (m *Manager) cachedOrFreshOptions(ctx context.Context, pluginID, streamID, owner string) (Options, error) {
	m.mu.Lock()
	c, ok := m.options[owner+"\x00"+pluginID+"\x00"+streamID]
	m.mu.Unlock()
	if ok && m.now().Sub(c.at) <= optionsTTL {
		return c.opts, nil
	}
	return m.Options(ctx, pluginID, streamID, owner)
}

// ─── create ──────────────────────────────────────────────────────────────────

// ItemMeta is the display metadata of one download.
type ItemMeta struct {
	StreamID, MediaID, ParentID, Title string
	SeriesTitle, Poster                string
	SeasonNumber, EpisodeNumber        int32
}

// CreateRequest mirrors the proto's CreateDownloadRequest.
type CreateRequest struct {
	Owner, Device, PluginID string
	ItemMeta
	VariantID             string
	AudioIDs, SubtitleIDs []string
	NotBefore             int64
	UsePreferredHours     bool
	UpgradeIfBetter       bool
}

// BatchRequest mirrors CreateDownloadsRequest: several streams, one choice.
type BatchRequest struct {
	Owner, Device, PluginID string
	Items                   []ItemMeta
	VariantID               string
	AudioIDs, SubtitleIDs   []string
	NotBefore               int64
	UsePreferredHours       bool
	UpgradeIfBetter         bool
}

// choice is a validated selection with its estimate and display labels.
type choice struct {
	sel                  Selection
	label                string
	est                  int64
	audioLangs, subLangs []string
}

func resolveChoice(opts Options, variantID string, audioIDs, subtitleIDs []string) (choice, error) {
	var c choice
	chosen := opts.Variants[0]
	if variantID != "" {
		if _, _, ok := parseVariantID(variantID); !ok {
			return c, ErrUnavailable{"qualità non valida"}
		}
		for _, v := range opts.Variants {
			if v.ID == variantID {
				chosen = v
			}
		}
	}
	c.sel.Height, c.sel.Bandwidth = chosen.Height, chosen.Bandwidth
	c.label, c.est = chosen.Label, chosen.EstimatedBytes
	for _, id := range audioIDs {
		t, ok := parseTrackID(id, "a")
		if !ok {
			return c, ErrUnavailable{"traccia audio non valida"}
		}
		c.sel.Audio = append(c.sel.Audio, t)
		c.audioLangs = append(c.audioLangs, trackLabel(t))
		c.est += estimateBytes(assumedAudioBitrate, opts.DurationSec)
	}
	for _, id := range subtitleIDs {
		t, ok := parseTrackID(id, "s")
		if !ok {
			return c, ErrUnavailable{"sottotitolo non valido"}
		}
		c.sel.Subtitles = append(c.sel.Subtitles, t)
		c.subLangs = append(c.subLangs, trackLabel(t))
	}
	return c, nil
}

func trackLabel(c TrackCriterion) string {
	if c.Language != "" {
		return c.Language
	}
	return c.Name
}

func dedupeKey(pluginID, streamID string, sel Selection) string {
	b, _ := json.Marshal(sel)
	return pluginID + "\x00" + streamID + "\x00" + string(b)
}

// schedule computes when a new download may start.
func (m *Manager) schedule(pluginID string, notBefore int64, useWindow bool, now time.Time) (int64, bool) {
	sched := max(notBefore, 0)
	if useWindow {
		if h, _ := preferredWindow(pluginID, now); h != "" {
			w, _ := parseWindow(h)
			start := now
			if sched > now.Unix() {
				start = time.Unix(sched, 0)
			}
			if ns := w.nextStart(start.In(time.Local)); ns.After(now) {
				sched = ns.Unix()
			}
			return sched, true
		}
	}
	return sched, false
}

// Create queues one download.
func (m *Manager) Create(ctx context.Context, r CreateRequest) (*Info, error) {
	opts, err := m.cachedOrFreshOptions(ctx, r.PluginID, r.StreamID, r.Owner)
	if err != nil {
		return nil, err
	}
	if !opts.Available {
		return nil, ErrUnavailable{opts.Reason}
	}
	c, err := resolveChoice(opts, r.VariantID, r.AudioIDs, r.SubtitleIDs)
	if err != nil {
		return nil, err
	}
	return m.createItem(r.Owner, r.Device, r.PluginID, r.ItemMeta, c, r.NotBefore, r.UsePreferredHours, r.UpgradeIfBetter, true)
}

// CreateBatch queues several streams with the choice made on the first
// one's options (ids are criteria, they apply to every episode). Only the
// first item is probed now; the others are checked when their job runs —
// probing a whole season up front would mean dozens of plugin resolves.
func (m *Manager) CreateBatch(ctx context.Context, r BatchRequest) ([]*Info, int64, error) {
	if len(r.Items) == 0 {
		return nil, 0, ErrUnavailable{"nessun contenuto da scaricare"}
	}
	if len(r.Items) > 200 {
		return nil, 0, ErrUnavailable{"troppi contenuti in una sola richiesta (max 200)"}
	}
	opts, err := m.cachedOrFreshOptions(ctx, r.PluginID, r.Items[0].StreamID, r.Owner)
	if err != nil {
		return nil, 0, err
	}
	if !opts.Available {
		return nil, 0, ErrUnavailable{opts.Reason}
	}
	c, err := resolveChoice(opts, r.VariantID, r.AudioIDs, r.SubtitleIDs)
	if err != nil {
		return nil, 0, err
	}
	// One space check for the whole batch, with the first item's estimate
	// standing in for each (episodes of one season are usually alike).
	total := c.est * int64(len(r.Items))
	if need := total + total/10; need > m.AvailableBytes() {
		return nil, total, spaceError(need, m.AvailableBytes())
	}
	var out []*Info
	for _, it := range r.Items {
		in, err := m.createItem(r.Owner, r.Device, r.PluginID, it, c, r.NotBefore, r.UsePreferredHours, r.UpgradeIfBetter, false)
		if err != nil {
			return out, total, err
		}
		out = append(out, in)
	}
	return out, total, nil
}

// createItem adds a profile's download, reusing an identical blob when one
// exists (same plugin, stream and choices — even another profile's).
func (m *Manager) createItem(owner, device, pluginID string, meta ItemMeta, c choice, notBefore int64,
	usePreferred, upgrade, checkSpace bool) (*Info, error) {
	m.createMu.Lock()
	defer m.createMu.Unlock()

	now := m.now()
	sched, useWindow := m.schedule(pluginID, notBefore, usePreferred, now)
	status := StatusQueued
	if sched > now.Unix() {
		status = StatusScheduled
	}
	key := dedupeKey(pluginID, meta.StreamID, c.sel)
	b, err := blobByKey(key)
	switch {
	case err == nil && b.Status != StatusFailed && b.Status != StatusCanceled:
		// Shared: bring a waiting job forward if this request wants it sooner.
		if (b.Status == StatusQueued || b.Status == StatusScheduled) && (sched < b.ScheduledFor || (b.UseWindow && !useWindow)) {
			setBlob(b.ID, `scheduled_for=?, use_window=?, status=?`, sched, boolInt(useWindow), status)
		}
		if upgrade && !b.Upgrade {
			setBlob(b.ID, `upgrade=1`)
			if b.Status == StatusCompleted {
				m.planUpgrade(b.ID, b.PluginID, b.Height, 0)
			}
		}
		log.Printf("[downloads] %s/%s già presente (blob %s): condiviso", pluginID, meta.StreamID, b.ID)
	default:
		if err == nil { // a failed/canceled attempt at the same content: start over
			m.deleteBlob(b)
		} else if !IsNotFound(err) {
			return nil, err
		}
		if checkSpace {
			if need := c.est + c.est/10; need > m.AvailableBytes() {
				return nil, spaceError(need, m.AvailableBytes())
			}
		}
		selJSON, _ := json.Marshal(c.sel)
		b = &blob{ID: newID()}
		_, err = managers.DB.Exec(`INSERT INTO dl_blobs(blob_id, dedupe_key, plugin_id, stream_id, owner_id, selection,
			status, estimated_bytes, quality_label, audio_languages, subtitle_languages, use_window, scheduled_for,
			created_at, upgrade) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			b.ID, key, pluginID, meta.StreamID, owner, string(selJSON), status, c.est, c.label,
			strings.Join(c.audioLangs, ","), strings.Join(c.subLangs, ","), boolInt(useWindow), sched,
			now.Unix(), boolInt(upgrade))
		if err != nil {
			return nil, err
		}
		log.Printf("[downloads] blob %s creato: %s/%s %s (~%s) stato=%s", b.ID, pluginID, meta.StreamID, c.label, humanBytes(c.est), status)
	}

	id := newID()
	_, err = managers.DB.Exec(`INSERT INTO dl_items(download_id, blob_id, owner_id, device_id, media_id, parent_id,
		title, series_title, poster, season_number, episode_number, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, b.ID, owner, device, meta.MediaID, meta.ParentID, meta.Title, meta.SeriesTitle, meta.Poster,
		meta.SeasonNumber, meta.EpisodeNumber, now.Unix())
	if err != nil {
		return nil, err
	}
	m.wake()
	return m.Get(owner, id)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ─── cancel / delete / ack ───────────────────────────────────────────────────

// Cancel stops a download that isn't finished. When its file is shared with
// another profile's download, only this profile's entry goes.
func (m *Manager) Cancel(owner, id string) error {
	it, err := loadItem(owner, id)
	if err != nil {
		return err
	}
	b, err := loadBlob(it.BlobID)
	if err != nil {
		return err
	}
	switch b.Status {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return nil
	}
	if itemCount(b.ID) > 1 {
		_, err = managers.DB.Exec(`DELETE FROM dl_items WHERE download_id=?`, id)
		return err
	}
	log.Printf("[downloads] blob %s annullato (download %s)", b.ID, id)
	m.stopLive(b.ID)
	setBlob(b.ID, `status=?, error=''`, StatusCanceled)
	m.removeBlobFiles(b.ID)
	return nil
}

// Delete removes a profile's download; the file goes when nobody else uses it.
func (m *Manager) Delete(owner, id string) error {
	it, err := loadItem(owner, id)
	if err != nil {
		return err
	}
	if _, err := managers.DB.Exec(`DELETE FROM dl_items WHERE download_id=?`, id); err != nil {
		return err
	}
	if itemCount(it.BlobID) == 0 {
		if b, err := loadBlob(it.BlobID); err == nil {
			log.Printf("[downloads] blob %s eliminato (download %s, stato %s)", b.ID, id, b.Status)
			m.deleteBlob(b)
		}
	}
	return nil
}

// DeleteAdmin is Delete without the owner check (dashboard).
func (m *Manager) DeleteAdmin(id string) error {
	it, err := loadItem("", id)
	if err != nil {
		return err
	}
	return m.Delete(it.Owner, id)
}

// Ack records that the device has the whole file. With "delete after the
// device fetched it" on, this profile's server copy goes right away (the
// file itself only if no other profile's download uses it).
func (m *Manager) Ack(owner, id string) error {
	if _, err := loadItem(owner, id); err != nil {
		return err
	}
	if _, err := managers.DB.Exec(`UPDATE dl_items SET fetched_at=? WHERE download_id=?`, m.now().Unix(), id); err != nil {
		return err
	}
	if deleteAfterFetch() {
		log.Printf("[downloads] %s consegnato al dispositivo: rimosso dal server", id)
		return m.Delete(owner, id)
	}
	return nil
}

// deleteBlob removes a blob, its items and its files.
func (m *Manager) deleteBlob(b *blob) {
	m.stopLive(b.ID)
	m.removeBlobFiles(b.ID)
	_, _ = managers.DB.Exec(`DELETE FROM dl_items WHERE blob_id=?`, b.ID)
	_, _ = managers.DB.Exec(`DELETE FROM dl_blobs WHERE blob_id=?`, b.ID)
}

func (m *Manager) stopLive(blobID string) {
	m.mu.Lock()
	lj := m.live[blobID]
	m.mu.Unlock()
	if lj != nil {
		lj.cancel()
	}
}

// recoverAfterRestart requeues jobs a restart interrupted; they resume from
// the segments already in their work directory. Half-written ffmpeg outputs
// are removed (muxing just restarts).
func (m *Manager) recoverAfterRestart() {
	res, err := managers.DB.Exec(`UPDATE dl_blobs SET status=? WHERE status IN (?,?)`, StatusQueued, StatusRunning, StatusPaused)
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[downloads] %d download interrotti dal riavvio: riprendono da dove erano arrivati", n)
		}
	}
	parts, _ := filepath.Glob(filepath.Join(m.dir, "*.part"))
	for _, p := range parts {
		_ = os.Remove(p)
	}
}

// ─── scheduler ───────────────────────────────────────────────────────────────

func (m *Manager) schedulerLoop(ctx context.Context) {
	t := time.NewTicker(schedulerTick)
	defer t.Stop()
	for {
		m.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
		}
	}
}

// dispatch starts due jobs and upgrade checks up to the concurrency limit.
// Nothing new starts while someone is watching (when that setting is on).
func (m *Manager) dispatch(ctx context.Context) {
	m.mu.Lock()
	free := concurrency() - len(m.live)
	m.mu.Unlock()
	if free <= 0 || (pauseWhileStreaming() && streamingActive()) {
		return
	}
	now := m.now()
	for _, b := range blobsWhere(`status IN ('queued','scheduled') AND scheduled_for<=? ORDER BY created_at LIMIT 20`, now.Unix()) {
		if free == 0 {
			return
		}
		if b.UseWindow {
			if h, _ := preferredWindow(b.PluginID, now); h != "" {
				if w, ok := parseWindow(h); ok && !w.contains(now.In(time.Local)) {
					// Window closed again (e.g. retry after a failure): wait for the next one.
					setBlob(b.ID, `status=?, scheduled_for=?`, StatusScheduled, w.nextStart(now.In(time.Local)).Unix())
					continue
				}
			}
		}
		if m.start(ctx, b, false) {
			free--
		}
	}
	for _, b := range blobsWhere(`status='completed' AND upgrade=1 AND upgrade_next>0 AND upgrade_next<=? ORDER BY upgrade_next LIMIT 5`, now.Unix()) {
		if free == 0 {
			return
		}
		if m.start(ctx, b, true) {
			free--
		}
	}
}

func (m *Manager) start(parent context.Context, b *blob, upgrade bool) bool {
	ctx, cancel := context.WithCancel(parent)
	m.mu.Lock()
	if _, busy := m.live[b.ID]; busy {
		m.mu.Unlock()
		cancel()
		return false
	}
	m.live[b.ID] = &liveJob{cancel: cancel, upgrade: upgrade}
	m.mu.Unlock()
	if !upgrade {
		setBlob(b.ID, `status=?, started_at=?, error='', attempts=attempts+1`, StatusRunning, m.now().Unix())
		b.Attempts++
	}

	core.SafeGo("downloads/job", func() {
		defer func() {
			m.mu.Lock()
			delete(m.live, b.ID)
			m.mu.Unlock()
			cancel()
			m.wake()
		}()
		err := m.runJob(ctx, b, upgrade)
		m.finish(b, upgrade, err, ctx.Err() != nil)
	})
	return true
}

var errNoBetter = errors.New("nessuna qualità migliore disponibile per ora")

func (m *Manager) finish(b *blob, upgrade bool, err error, canceled bool) {
	if upgrade {
		m.finishUpgrade(b, err, canceled)
		return
	}
	switch {
	case err == nil:
		log.Printf("[downloads] blob %s completato", b.ID)
		if cur, _ := loadBlob(b.ID); cur != nil && cur.Upgrade {
			m.planUpgrade(b.ID, b.PluginID, cur.Height, 0)
		}
	case canceled:
		// Cancel/Delete already set the final state.
	default:
		var ua ErrUnavailable
		if errors.As(err, &ua) || b.Attempts >= maxAttempts {
			setBlob(b.ID, `status=?, error=?`, StatusFailed, err.Error())
			m.removeBlobFiles(b.ID)
			log.Printf("[downloads] blob %s fallito: %v", b.ID, err)
			return
		}
		// Keep the work dir (the next attempt resumes from it) and back off for
		// real: a source that stopped answering needs time.
		retryAt := m.now().Add(retryDelay(b.Attempts))
		setBlob(b.ID, `status=?, error=?, scheduled_for=?`, StatusScheduled, retryMessage(err, retryAt), retryAt.Unix())
		log.Printf("[downloads] blob %s tentativo %d/%d fallito, riprendo dal punto raggiunto alle %s: %v",
			b.ID, b.Attempts, maxAttempts, retryAt.Format("02/01 15:04"), err)
	}
}

// retryDelay is the wait after the n-th failed attempt: 20 min, 1 h, 3 h,
// 6 h, 12 h. With resume, waiting costs nothing but time.
func retryDelay(n int) time.Duration {
	delays := []time.Duration{20 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour}
	if n < 1 {
		n = 1
	}
	if n > len(delays) {
		n = len(delays)
	}
	return delays[n-1]
}

// retryMessage is the user-facing state of a download waiting to retry.
func retryMessage(err error, at time.Time) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "HTTP 502"), strings.Contains(msg, "HTTP 504"), strings.Contains(msg, "HTTP 503"):
		msg = "la sorgente non risponde"
	case strings.Contains(msg, "HTTP 429"), strings.Contains(msg, "HTTP 403"):
		msg = "la sorgente sta limitando le richieste"
	}
	return "Interrotto (" + msg + "): riprendo da dove ero arrivato alle " + at.Format("15:04")
}

// planUpgrade schedules the next quality check of a finished file, if it's
// below what the source usually serves (and attempts remain).
func (m *Manager) planUpgrade(blobID, pluginID string, height, attempts int) {
	now := m.now()
	usual := usualMaxHeight(pluginID, now)
	if height <= 0 || usual <= height || attempts >= maxUpgradeAttempts {
		setBlob(blobID, `upgrade_next=0`)
		return
	}
	next := now.Add(24 * time.Hour)
	if h, _ := preferredWindow(pluginID, now); h != "" {
		if w, ok := parseWindow(h); ok {
			// Not the current window again: the file was just made in it.
			next = w.nextStart(now.Add(time.Hour).In(time.Local))
		}
	}
	setBlob(blobID, `upgrade_next=?`, next.Unix())
	log.Printf("[downloads] blob %s a %dp (di solito %dp): nuovo tentativo di qualità alle %s", blobID, height, usual, next.Format("02/01 15:04"))
}

func (m *Manager) finishUpgrade(b *blob, err error, canceled bool) {
	_ = os.Remove(m.upPartPath(b.ID))
	_ = os.RemoveAll(m.upWorkDir(b.ID))
	cur, lerr := loadBlob(b.ID)
	if lerr != nil || canceled {
		return
	}
	if err == nil {
		setBlob(b.ID, `upgrade=0, upgrade_next=0`)
		log.Printf("[downloads] blob %s sostituito con una qualità migliore (%s)", b.ID, cur.QualityLabel)
		return
	}
	attempts := cur.UpgradeAttempts + 1
	setBlob(b.ID, `upgrade_attempts=?`, attempts)
	if !errors.Is(err, errNoBetter) {
		log.Printf("[downloads] blob %s tentativo di miglioramento fallito: %v", b.ID, err)
	}
	m.planUpgrade(b.ID, b.PluginID, cur.Height, attempts)
}

// ─── cleanup ─────────────────────────────────────────────────────────────────

func (m *Manager) cleanupLoop(ctx context.Context) {
	t := time.NewTicker(cleanupTick)
	defer t.Stop()
	for {
		m.cleanup()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// cleanup deletes files past retention, blobs nobody uses any more and old
// failed/canceled attempts.
func (m *Manager) cleanup() {
	now := m.now()
	if days := retentionDays(); days > 0 {
		cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).Unix()
		for _, b := range blobsWhere(`status=? AND completed_at>0 AND completed_at<?`, StatusCompleted, cutoff) {
			m.deleteBlob(b)
			log.Printf("[downloads] blob %s scaduto dopo %d giorni, rimosso", b.ID, days)
		}
	}
	for _, b := range blobsWhere(`blob_id NOT IN (SELECT blob_id FROM dl_items)`) {
		m.deleteBlob(b)
	}
	old := now.Add(-7 * 24 * time.Hour).Unix()
	for _, b := range blobsWhere(`status IN (?,?) AND created_at<?`, StatusFailed, StatusCanceled, old) {
		m.deleteBlob(b)
	}
}

// ─── live state ──────────────────────────────────────────────────────────────

func (m *Manager) setProgress(blobID string, p float64, bytes int64) {
	m.mu.Lock()
	if lj := m.live[blobID]; lj != nil {
		lj.progress, lj.bytes = p, bytes
	}
	m.mu.Unlock()
}

func (m *Manager) setPaused(blobID string, paused bool) {
	m.mu.Lock()
	lj := m.live[blobID]
	changed := lj != nil && lj.paused != paused
	if changed {
		lj.paused = paused
	}
	upgrade := lj != nil && lj.upgrade
	m.mu.Unlock()
	if changed && !upgrade {
		status := StatusRunning
		if paused {
			status = StatusPaused
		}
		setBlob(blobID, `status=?`, status)
	}
}

// liveState is the in-memory progress of a running (non-upgrade) job.
func (m *Manager) liveState(blobID string) (progress float64, bytes int64, paused, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lj := m.live[blobID]
	if lj == nil || lj.upgrade {
		return 0, 0, false, false
	}
	return lj.progress, lj.bytes, lj.paused, true
}

// ─── speed ───────────────────────────────────────────────────────────────────

// recentBytesPerSec averages the throughput of the last completed downloads.
func recentBytesPerSec() int64 {
	var bps sql.NullFloat64
	_ = managers.DB.QueryRow(`SELECT AVG(CAST(file_bytes AS REAL) / (completed_at - started_at)) FROM (
		SELECT file_bytes, completed_at, started_at FROM dl_blobs
		WHERE status=? AND completed_at > started_at AND started_at > 0
		ORDER BY completed_at DESC LIMIT 5)`, StatusCompleted).Scan(&bps)
	if !bps.Valid {
		return 0
	}
	return int64(bps.Float64)
}
