package downloads

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/engine"
	"mycelium/internal/managers"

	_ "modernc.org/sqlite"
)

// env is a Manager on an in-memory DB with a fake job body (no ffmpeg).
type env struct {
	m         *Manager
	up        *httptest.Server
	mu        sync.Mutex
	settings  map[string]string
	runs      int
	streaming atomic.Bool
	hours     string // plugin preferred_hours
	better    bool   // upgrade jobs find a better quality
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	prevDB := managers.DB
	managers.DB = managers.NewDBManager(db)

	e := &env{settings: map[string]string{}, hours: "02:00-07:00"}
	prevSetting, prevCfg, prevStreaming := setting, pluginDownloadConfig, streamingActive
	setting = func(k, def string) string {
		e.mu.Lock()
		defer e.mu.Unlock()
		if v, ok := e.settings[k]; ok {
			return v
		}
		return def
	}
	pluginDownloadConfig = func(id string) engine.LuaManifestDownload {
		e.mu.Lock()
		defer e.mu.Unlock()
		return engine.LuaManifestDownload{Enabled: id == "example", PreferredHours: e.hours, Note: "di sera 720p"}
	}
	streamingActive = func() bool { return e.streaming.Load() }

	e.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "master.m3u8") {
			io.WriteString(w, testMaster)
			return
		}
		io.WriteString(w, testMedia)
	}))
	resolve := func(ctx context.Context, pluginID, streamID, owner string) (Source, error) {
		return Source{URL: e.up.URL + "/hls/master.m3u8", IsHLS: true, Extra: map[string]string{"download_notice": "ora 1080p"}}, nil
	}
	m, err := Init(t.TempDir(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	m.ffmpeg = "/fake/ffmpeg"
	m.runJob = func(ctx context.Context, b *blob, upgrade bool) error {
		e.mu.Lock()
		e.runs++
		better := e.better
		e.mu.Unlock()
		if upgrade {
			if !better {
				return errNoBetter
			}
			setBlob(b.ID, `height=1080, quality_label='1080p'`)
			return os.WriteFile(m.finalPath(b.ID), []byte(strings.Repeat("y", 8192)), 0o644)
		}
		if err := os.WriteFile(m.finalPath(b.ID), []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
			return err
		}
		setBlob(b.ID, `status=?, progress=1, file_bytes=4096, completed_at=?, height=?`,
			StatusCompleted, m.now().Unix(), b.Sel.Height)
		return nil
	}
	e.m = m
	t.Cleanup(func() {
		e.waitIdle(t)
		e.up.Close()
		db.Close()
		managers.DB = prevDB
		setting, pluginDownloadConfig, streamingActive = prevSetting, prevCfg, prevStreaming
	})
	return e
}

func (e *env) set(k, v string) {
	e.mu.Lock()
	e.settings[k] = v
	e.mu.Unlock()
}

func (e *env) runCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs
}

// waitIdle waits until no job is running (never swap the clock under one).
func (e *env) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.m.mu.Lock()
		n := len(e.m.live)
		e.m.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job still running")
}

// waitStatus runs the scheduler until id reaches status.
func (e *env) waitStatus(t *testing.T, owner, id, status string) *Info {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.m.dispatch(context.Background())
		e.waitIdle(t)
		if in, err := e.m.Get(owner, id); err == nil && in.Status == status {
			return in
		}
	}
	in, _ := e.m.Get(owner, id)
	t.Fatalf("status %q never reached: %+v", status, in)
	return nil
}

func (e *env) create(t *testing.T, owner string, opts ...func(*CreateRequest)) *Info {
	t.Helper()
	r := CreateRequest{Owner: owner, PluginID: "example", ItemMeta: ItemMeta{StreamID: "s1", Title: "Ep 1"}}
	for _, o := range opts {
		o(&r)
	}
	in, err := e.m.Create(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestManager_OptionsCreateRunServe(t *testing.T) {
	e := newEnv(t)
	SetSignKey([]byte("k"))
	defer SetSignKey(nil)

	o, err := e.m.Options(context.Background(), "example", "s1", "prof")
	if err != nil || !o.Available {
		t.Fatalf("options: %+v %v", o, err)
	}
	if o.DurationSec != 15.5 || len(o.Variants) != 3 || len(o.Audio) != 2 || len(o.Subtitles) != 1 {
		t.Fatalf("options content %+v", o)
	}
	if o.PluginNotice != "ora 1080p" || o.PreferredHours != "02:00-07:00" || o.PreferredHoursSource != "plugin" {
		t.Fatalf("hints %+v", o)
	}
	if u, _ := e.m.Options(context.Background(), "media-server", "s1", "prof"); u.Available {
		t.Fatal("plugin without download section offered downloads")
	}

	in := e.create(t, "prof", func(r *CreateRequest) {
		r.VariantID, r.AudioIDs, r.SubtitleIDs = o.Variants[1].ID, []string{o.Audio[1].ID}, []string{o.Subtitles[0].ID}
	})
	if in.Status != StatusQueued || in.QualityLabel != "720p" || strings.Join(in.AudioLangs, ",") != "jpn" {
		t.Fatalf("created %+v", in)
	}
	done := e.waitStatus(t, "prof", in.ID, StatusCompleted)
	if done.FilePath == "" || done.ExpiresAt == 0 {
		t.Fatalf("completed %+v", done)
	}
	if _, err := e.m.Get("other", in.ID); !IsNotFound(err) {
		t.Fatal("download visible to another owner")
	}
	req := httptest.NewRequest(http.MethodGet, done.FilePath, nil)
	req.Header.Set("Range", "bytes=0-99")
	rec := httptest.NewRecorder()
	e.m.ServeFile(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != 100 {
		t.Fatalf("serve: %d %d", rec.Code, rec.Body.Len())
	}
	// The file replaced under the same id (quality upgrade): the old link
	// must not keep serving Range requests over the new bytes.
	it, err := loadItem("prof", in.ID)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(e.m.finalPath(it.BlobID), later, later); err != nil {
		t.Fatal(err)
	}
	gone := httptest.NewRecorder()
	e.m.ServeFile(gone, req)
	if gone.Code != http.StatusGone {
		t.Fatalf("stale link after replacement: %d", gone.Code)
	}
	bad := httptest.NewRecorder()
	e.m.ServeFile(bad, httptest.NewRequest(http.MethodGet, "/downloads/file?id="+in.ID, nil))
	if bad.Code != http.StatusForbidden {
		t.Fatalf("unsigned fetch: %d", bad.Code)
	}
}

// Two profiles, same episode, same choices: one blob, one file, one fetch.
func TestManager_DedupeAcrossProfiles(t *testing.T) {
	e := newEnv(t)
	a := e.create(t, "anna")
	e.waitStatus(t, "anna", a.ID, StatusCompleted)
	b := e.create(t, "bruno")
	if b.Status != StatusCompleted || !b.Shared {
		t.Fatalf("second profile didn't reuse the finished file: %+v", b)
	}
	if e.runCount() != 1 {
		t.Fatalf("content fetched %d times", e.runCount())
	}
	it, _ := loadItem("", a.ID)
	if err := e.m.Delete("anna", a.ID); err != nil {
		t.Fatal(err)
	}
	if !fileExists(e.m.finalPath(it.BlobID)) {
		t.Fatal("file removed while another profile still has it")
	}
	if err := e.m.Delete("bruno", b.ID); err != nil {
		t.Fatal(err)
	}
	if fileExists(e.m.finalPath(it.BlobID)) {
		t.Fatal("file left behind after the last download went")
	}
	// A different choice is a different file.
	c := e.create(t, "anna", func(r *CreateRequest) { r.VariantID = variantID(Variant{Height: 480, Bandwidth: 900000}) })
	d := e.create(t, "bruno")
	ci, _ := loadItem("", c.ID)
	di, _ := loadItem("", d.ID)
	if ci.BlobID == di.BlobID {
		t.Fatal("different qualities shared a file")
	}
}

func TestManager_AckDeletesWhenConfigured(t *testing.T) {
	e := newEnv(t)
	in := e.create(t, "p")
	e.waitStatus(t, "p", in.ID, StatusCompleted)
	if err := e.m.Ack("p", in.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.m.Get("p", in.ID); got == nil || !got.Fetched {
		t.Fatal("ack not recorded (delete-after-fetch is off by default)")
	}
	e.set("download_delete_after_fetch", "1")
	it, _ := loadItem("", in.ID)
	if err := e.m.Ack("p", in.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Get("p", in.ID); !IsNotFound(err) || fileExists(e.m.finalPath(it.BlobID)) {
		t.Fatal("server copy kept after the device confirmed it")
	}
}

func TestManager_WaitsWhileSomeoneWatches(t *testing.T) {
	e := newEnv(t)
	e.streaming.Store(true)
	in := e.create(t, "p")
	e.m.dispatch(context.Background())
	e.waitIdle(t)
	if e.runCount() != 0 {
		t.Fatal("download started while someone was watching")
	}
	e.set("download_pause_while_streaming", "0")
	e.waitStatus(t, "p", in.ID, StatusCompleted)
}

// A running job holds between segments while someone watches, shown as
// "paused", and continues by itself.
func TestWaitWhileStreaming_PausesAndResumes(t *testing.T) {
	e := newEnv(t)
	e.streaming.Store(true)
	b := &blob{ID: "b1"}
	_, _ = managers.DB.Exec(`INSERT INTO dl_blobs(blob_id, dedupe_key, plugin_id, stream_id, status, created_at) VALUES('b1','k','example','s','running',0)`)
	e.m.mu.Lock()
	e.m.live[b.ID] = &liveJob{cancel: func() {}}
	e.m.mu.Unlock()
	defer func() { e.m.mu.Lock(); delete(e.m.live, b.ID); e.m.mu.Unlock() }()

	done := make(chan error, 1)
	go func() { done <- e.m.waitWhileStreaming(context.Background(), b.ID) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got, _ := loadBlob("b1"); got != nil && got.Status == StatusPaused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never paused")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.streaming.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("never resumed")
	}
	if got, _ := loadBlob("b1"); got.Status != StatusRunning {
		t.Fatalf("status after resume %q", got.Status)
	}
}

func TestManager_PreferredHoursSchedules(t *testing.T) {
	e := newEnv(t)
	noon := time.Date(2026, 9, 29, 12, 0, 0, 0, time.Local)
	e.m.now = func() time.Time { return noon }
	in := e.create(t, "p", func(r *CreateRequest) { r.UsePreferredHours = true })
	if want := time.Date(2026, 9, 30, 2, 0, 0, 0, time.Local).Unix(); in.Status != StatusScheduled || in.ScheduledFor != want {
		t.Fatalf("scheduled %+v, want at %d", in, want)
	}
	e.m.dispatch(context.Background())
	e.waitIdle(t)
	if e.runCount() != 0 {
		t.Fatal("scheduled download started before its window")
	}
	e.m.now = func() time.Time { return time.Date(2026, 9, 30, 2, 30, 0, 0, time.Local) }
	e.waitStatus(t, "p", in.ID, StatusCompleted)
}

func TestLearnedWindow(t *testing.T) {
	newEnv(t)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.Local)
	at := func(h int) time.Time { return time.Date(2026, 9, 29, h, 10, 0, 0, time.Local) }
	for h := 0; h < 24; h++ {
		height := 720
		if h >= 1 && h < 7 {
			height = 1080
		}
		recordQuality("example", height, at(h))
	}
	if got := learnedWindow("example", now); got != "01:00-07:00" {
		t.Fatalf("learned window %q", got)
	}
	if got := usualMaxHeight("example", now); got != 1080 {
		t.Fatalf("usual max %d", got)
	}
	// Constant quality: nothing to suggest. Too little data: nothing either.
	for h := 0; h < 24; h++ {
		recordQuality("flat", 1080, at(h))
	}
	recordQuality("sparse", 1080, at(3))
	if learnedWindow("flat", now) != "" || learnedWindow("sparse", now) != "" {
		t.Fatal("window suggested without reason")
	}
	// Old observations are forgotten.
	if got := learnedWindow("example", now.Add(30*24*time.Hour)); got != "" {
		t.Fatalf("stale window %q", got)
	}
}

func TestManager_LearnedWindowUsedWhenPluginHasNone(t *testing.T) {
	e := newEnv(t)
	e.hours = ""
	for h := 0; h < 24; h++ {
		height := 720
		if h >= 3 && h < 5 {
			height = 1080
		}
		recordQuality("example", height, time.Date(2026, 9, 29, h, 0, 0, 0, time.Local))
	}
	e.m.now = func() time.Time { return time.Date(2026, 9, 29, 20, 0, 0, 0, time.Local) }
	o, err := e.m.Options(context.Background(), "example", "s1", "p")
	if err != nil {
		t.Fatal(err)
	}
	// The test master offers 1080p right now: learned window, but not "below usual".
	if o.PreferredHours != "03:00-05:00" || o.PreferredHoursSource != "learned" || o.QualityBelowUsual {
		t.Fatalf("learned hints %+v", o)
	}
}

func TestManager_UpgradeReplacesWithBetterQuality(t *testing.T) {
	e := newEnv(t)
	for h := 0; h < 24; h++ {
		recordQuality("example", 1080, time.Date(2026, 9, 29, h, 0, 0, 0, time.Local))
	}
	start := time.Date(2026, 9, 29, 20, 0, 0, 0, time.Local)
	e.m.now = func() time.Time { return start }
	in := e.create(t, "p", func(r *CreateRequest) {
		r.VariantID, r.UpgradeIfBetter = variantID(Variant{Height: 720, Bandwidth: 2500000}), true
	})
	done := e.waitStatus(t, "p", in.ID, StatusCompleted)
	if !done.UpgradePending {
		t.Fatalf("720p file below the usual 1080p: no upgrade planned %+v", done)
	}
	it, _ := loadItem("", in.ID)
	b, _ := loadBlob(it.BlobID)

	// First try: nothing better yet → rescheduled.
	e.m.now = func() time.Time { return time.Unix(b.UpgradeNext, 0).Add(time.Minute) }
	e.m.dispatch(context.Background())
	e.waitIdle(t)
	b2, _ := loadBlob(it.BlobID)
	if b2.UpgradeAttempts != 1 || b2.UpgradeNext <= b.UpgradeNext {
		t.Fatalf("not rescheduled: %+v", b2)
	}
	// Second try: a better quality is there → file replaced, upgrade done.
	e.mu.Lock()
	e.better = true
	e.mu.Unlock()
	e.m.now = func() time.Time { return time.Unix(b2.UpgradeNext, 0).Add(time.Minute) }
	e.m.dispatch(context.Background())
	e.waitIdle(t)
	got, _ := e.m.Get("p", in.ID)
	if got.UpgradePending || got.QualityLabel != "1080p" || got.Status != StatusCompleted {
		t.Fatalf("after upgrade %+v", got)
	}
	if fi, _ := os.Stat(e.m.finalPath(it.BlobID)); fi.Size() != 8192 {
		t.Fatal("file not replaced")
	}
}

func TestManager_BatchSeason(t *testing.T) {
	e := newEnv(t)
	o, _ := e.m.Options(context.Background(), "example", "e1", "p")
	var items []ItemMeta
	for i := 1; i <= 3; i++ {
		items = append(items, ItemMeta{StreamID: "e" + string(rune('0'+i)), SeriesTitle: "Serie", SeasonNumber: 1, EpisodeNumber: int32(i)})
	}
	ins, total, err := e.m.CreateBatch(context.Background(), BatchRequest{Owner: "p", PluginID: "example", Items: items,
		VariantID: o.Variants[2].ID, AudioIDs: []string{o.Audio[0].ID}})
	if err != nil || len(ins) != 3 {
		t.Fatalf("batch: %v %d", err, len(ins))
	}
	if want := 3 * (o.Variants[2].EstimatedBytes + o.Audio[0].EstimatedBytes); total != want {
		t.Fatalf("total %d want %d", total, want)
	}
	for _, in := range ins {
		if in.QualityLabel != "480p" {
			t.Fatalf("choice not applied to every episode: %+v", in)
		}
	}
	e.set("download_quota_gb", "0.000001")
	if _, _, err := e.m.CreateBatch(context.Background(), BatchRequest{Owner: "p", PluginID: "example", Items: items[:1]}); err == nil {
		t.Fatal("over-quota batch accepted")
	}
}

func TestManager_RefusesWhenOverQuota(t *testing.T) {
	e := newEnv(t)
	e.set("download_quota_gb", "0.000001") // 1 KB
	_, err := e.m.Create(context.Background(), CreateRequest{Owner: "p", PluginID: "example", ItemMeta: ItemMeta{StreamID: "s1"}})
	var ua ErrUnavailable
	if !errors.As(err, &ua) || !strings.Contains(ua.Reason, "spazio insufficiente") {
		t.Fatalf("over-quota create: %v", err)
	}
}

func TestManager_RetryThenFail(t *testing.T) {
	e := newEnv(t)
	e.m.runJob = func(ctx context.Context, b *blob, upgrade bool) error { return errors.New("upstream 503") }
	in := e.create(t, "p")
	base := time.Now()
	for i := 0; i < maxAttempts; i++ {
		e.waitIdle(t)
		at := base.Add(time.Duration(i) * 24 * time.Hour) // past every backoff
		e.m.now = func() time.Time { return at }
		e.m.dispatch(context.Background())
	}
	e.waitIdle(t)
	got, _ := e.m.Get("p", in.ID)
	if got.Status != StatusFailed || !strings.Contains(got.Error, "upstream 503") {
		t.Fatalf("after %d failures: %+v", maxAttempts, got)
	}
}

func TestManager_RetentionAndRestartKeepsWork(t *testing.T) {
	e := newEnv(t)
	in := e.create(t, "p")
	e.waitStatus(t, "p", in.ID, StatusCompleted)
	e.m.now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	e.m.cleanup()
	if _, err := e.m.Get("p", in.ID); !IsNotFound(err) {
		t.Fatal("expired download not removed")
	}
	e.m.now = time.Now

	// A job left running by a restart goes back to the queue with its
	// fetched segments; only the half-written mux output goes.
	in2 := e.create(t, "p", func(r *CreateRequest) { r.StreamID = "s2" })
	it, _ := loadItem("", in2.ID)
	setBlob(it.BlobID, `status='running'`)
	_ = os.MkdirAll(e.m.workDir(it.BlobID), 0o755)
	seg := filepath.Join(e.m.workDir(it.BlobID), "i0-s00000.seg")
	_ = os.WriteFile(seg, []byte("x"), 0o644)
	_ = os.WriteFile(e.m.partPath(it.BlobID), []byte("x"), 0o644)
	e.m.recoverAfterRestart()
	if got, _ := e.m.Get("p", in2.ID); got.Status != StatusQueued {
		t.Fatalf("after restart: %+v", got)
	}
	if !fileExists(seg) || fileExists(e.m.partPath(it.BlobID)) {
		t.Fatal("restart must keep fetched segments and drop the partial output")
	}
}

func TestRetryBackoffAndMessage(t *testing.T) {
	if retryDelay(1) != 20*time.Minute || retryDelay(3) != 3*time.Hour || retryDelay(99) != 12*time.Hour {
		t.Fatal("backoff schedule")
	}
	at := time.Date(2026, 9, 29, 19, 30, 0, 0, time.Local)
	msg := retryMessage(errors.New("video i0-s00138.seg: HTTP 502"), at)
	if !strings.Contains(msg, "la sorgente non risponde") || !strings.Contains(msg, "19:30") {
		t.Fatalf("message %q", msg)
	}
}

func TestPacer(t *testing.T) {
	seg := func(end float64) localResource { return localResource{Kind: "segment", End: end} }
	p := pacer{factor: 4}
	t0 := time.Now()
	// Resumed job: 10 segments of 6 s already on disk don't count.
	for i := 1; i <= 10; i++ {
		p.skip(seg(float64(i) * 6))
	}
	if d := p.delay(seg(66), t0); d != 0 {
		t.Fatalf("first fetched segment waited %v", d)
	}
	p.lastEnd = 66
	// Next one: 6 s of content at 4× = 1.5 s after the start.
	if d := p.delay(seg(72), t0.Add(500*time.Millisecond)); d != time.Second {
		t.Fatalf("second segment wait %v, want 1s", d)
	}
	if d := p.delay(localResource{Kind: "key"}, t0); d != 0 {
		t.Fatal("keys must not be paced")
	}
	off := pacer{factor: 0}
	off.delay(seg(6), t0)
	if d := off.delay(seg(600), t0); d != 0 {
		t.Fatal("pace 0 must mean unlimited")
	}
}
