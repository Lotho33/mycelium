package downloads

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mycelium/internal/managers"
)

// ffInput is one ffmpeg input: the video variant, or a separate audio /
// subtitle rendition.
type ffInput struct {
	URL      string
	Kind     string // "video" | "audio" | "subtitle"
	Language string
	Name     string
}

// plan picks, from a freshly resolved master, the inputs matching sel.
func plan(master Master, sel Selection) (Variant, []ffInput) {
	v, _ := pickVariant(master.Variants, sel)
	inputs := []ffInput{{URL: v.URI, Kind: "video"}}

	audio := master.groupRenditions("AUDIO", v.Audio)
	var picked []Rendition
	seen := map[string]bool{}
	for _, c := range sel.Audio {
		if r, ok := pickRendition(audio, c); ok && r.URI != "" && !seen[r.URI] {
			picked = append(picked, r)
			seen[r.URI] = true
		}
	}
	if len(picked) == 0 && v.Audio != "" {
		// Nothing chosen or matched: take the group's default separate rendition,
		// or a video-only variant would have no sound.
		sort.SliceStable(audio, func(i, j int) bool { return audio[i].Default && !audio[j].Default })
		for _, r := range audio {
			if r.URI != "" {
				picked = append(picked, r)
				break
			}
		}
	}
	for _, r := range picked {
		inputs = append(inputs, ffInput{URL: r.URI, Kind: "audio", Language: r.Language, Name: r.Name})
	}

	subs := master.groupRenditions("SUBTITLES", v.Subtitles)
	seen = map[string]bool{}
	for _, c := range sel.Subtitles {
		if r, ok := pickRendition(subs, c); ok && r.URI != "" && !seen[r.URI] {
			inputs = append(inputs, ffInput{URL: r.URI, Kind: "subtitle", Language: r.Language, Name: r.Name})
			seen[r.URI] = true
		}
	}
	return v, inputs
}

// muxInputOpts are the per-input options for the local playlists: segments
// and keys are local files with our own names/extensions, and a segment's
// content (TS, fMP4, AAC, WebVTT) needn't match its extension.
var muxInputOpts = []string{"-protocol_whitelist", "file,crypto", "-allowed_extensions", "ALL", "-extension_picky", "0"}

// buildFFmpegArgs muxes inputs — stream copy, no re-encoding — into one
// Matroska file. Subtitles are converted to SRT (text, tiny), which every
// player handles inside MKV.
func buildFFmpegArgs(inputs []ffInput, inputOpts []string, out, title string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-nostats", "-progress", "pipe:1", "-y"}
	hasSeparateAudio := false
	for _, in := range inputs {
		args = append(args, inputOpts...)
		args = append(args, "-i", in.URL)
		if in.Kind == "audio" {
			hasSeparateAudio = true
		}
	}
	args = append(args, "-map", "0:v:0")
	if !hasSeparateAudio {
		args = append(args, "-map", "0:a?")
	}
	var aIdx, sIdx int
	var meta []string
	for i, in := range inputs {
		switch in.Kind {
		case "audio":
			args = append(args, "-map", fmt.Sprintf("%d:a:0", i))
			meta = append(meta, streamMeta("a", aIdx, in)...)
			aIdx++
		case "subtitle":
			args = append(args, "-map", fmt.Sprintf("%d:s:0?", i))
			meta = append(meta, streamMeta("s", sIdx, in)...)
			sIdx++
		}
	}
	args = append(args, "-c", "copy")
	if sIdx > 0 {
		args = append(args, "-c:s", "srt")
	}
	args = append(args, meta...)
	if aIdx > 0 {
		args = append(args, "-disposition:a:0", "default")
	}
	if title != "" {
		args = append(args, "-metadata", "title="+title)
	}
	return append(args, "-f", "matroska", out)
}

func streamMeta(kind string, idx int, in ffInput) []string {
	var out []string
	if in.Language != "" {
		out = append(out, fmt.Sprintf("-metadata:s:%s:%d", kind, idx), "language="+in.Language)
	}
	if in.Name != "" {
		out = append(out, fmt.Sprintf("-metadata:s:%s:%d", kind, idx), "title="+in.Name)
	}
	return out
}

// progressTracker reads ffmpeg's -progress key=value stream.
type progressTracker struct {
	onUpdate func(outSec float64, bytes int64, end bool)
}

func (p progressTracker) consume(r io.Reader) {
	sc := bufio.NewScanner(r)
	var outUS, size int64
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "out_time_us", "out_time_ms": // both are microseconds in ffmpeg's output
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > outUS {
				outUS = n
			}
		case "total_size":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				size = n
			}
		case "progress":
			p.onUpdate(float64(outUS)/1e6, size, v == "end")
		}
	}
}

// tailBuffer keeps the last bytes written (ffmpeg's error output).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > 2048 {
		t.buf = t.buf[len(t.buf)-2048:]
	}
	t.mu.Unlock()
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// ─── the job ─────────────────────────────────────────────────────────────────

// localInput is one input once its media playlist is fetched: the local
// playlist ffmpeg reads and the files it needs.
type localInput struct {
	ffInput
	remoteURL string
	prefix    string
	res       []localResource
	refreshes int
}

// workState records what a work directory holds, so a resumed job knows its
// files still match the stream (same inputs, same segment counts).
type workState struct {
	Inputs []string `json:"inputs"` // "<kind>:<language>:<segments>"
}

const fetchProgressShare = 0.95 // the rest is muxing

// runBlob produces (or, with upgrade, re-produces in a better quality) a
// blob's file: fetch every segment into the work dir — skipping those already
// there — then mux. Safe to interrupt at any point.
func (m *Manager) runBlob(ctx context.Context, b *blob, upgrade bool) error {
	src, master, dur, err := m.probe(ctx, b.PluginID, b.StreamID, b.Owner)
	if err != nil {
		return err
	}
	sel := b.Sel
	work, part := m.workDir(b.ID), m.partPath(b.ID)
	if upgrade {
		if master.Variants[0].Height <= b.Height {
			return errNoBetter
		}
		sel.Height, sel.Bandwidth = 0, 0 // the best there is now, same tracks
		work, part = m.upWorkDir(b.ID), m.upPartPath(b.ID)
	}
	v, inputs := plan(master, sel)
	est := estimateBytes(v.Bandwidth, dur)
	for _, in := range inputs {
		if in.Kind == "audio" {
			est += estimateBytes(assumedAudioBitrate, dur)
		}
	}
	if !upgrade {
		setBlob(b.ID, `estimated_bytes=?, duration_sec=?, quality_label=?, height=?`, est, dur, variantLabel(v), v.Height)
	}
	var headers map[string]string
	if !strings.Contains(src.URL, "/proxy/") {
		headers = src.Headers // direct_stream plugin: no proxy to add them
	}

	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	locals := make([]*localInput, len(inputs))
	for i, in := range inputs {
		li := &localInput{ffInput: in, remoteURL: in.URL, prefix: "i" + strconv.Itoa(i)}
		if err := m.loadInput(ctx, li, work, headers); err != nil {
			return err
		}
		locals[i] = li
	}
	if err := reconcileWork(work, locals); err != nil {
		return err
	}
	already := dirSize(work)
	if need := est + est/10 - already; need > m.AvailableBytes() {
		return spaceError(need, m.AvailableBytes())
	}

	// ── fetch ──
	fetched := already
	lastPersist := time.Time{}
	count := 0
	for _, li := range locals {
		pace := pacer{factor: paceFactor()}
		for ri := range li.res {
			path := filepath.Join(work, li.res[ri].Name)
			if fileExists(path) {
				pace.skip(li.res[ri])
				continue
			}
			if err := m.waitWhileStreaming(ctx, b.ID); err != nil {
				return err
			}
			if err := pace.wait(ctx, li.res[ri]); err != nil {
				return err
			}
			n, err := m.fetchResource(ctx, li, ri, work, headers)
			if err != nil {
				return err
			}
			fetched += n
			count++
			if !upgrade {
				frac := fetchProgressShare
				if est > 0 {
					frac = min(fetchProgressShare, fetchProgressShare*float64(fetched)/float64(est))
				}
				m.setProgress(b.ID, frac, fetched)
				if time.Since(lastPersist) > 10*time.Second {
					lastPersist = time.Now()
					setBlob(b.ID, `progress=?, file_bytes=?`, frac, fetched)
				}
			}
			// Stop before the quota or the disk margin is crossed.
			if count%10 == 0 && m.AvailableBytes() <= 0 {
				return ErrUnavailable{Reason: "spazio sul server esaurito durante il download (quota o margine del disco)"}
			}
		}
	}

	// ── mux ──
	if err := m.mux(ctx, b, locals, work, part, dur, fetched, upgrade); err != nil {
		return err
	}
	final := m.finalPath(b.ID)
	if err := os.Rename(part, final); err != nil { // replaces the old file on an upgrade
		return fmt.Errorf("finalizzazione file: %w", err)
	}
	_ = os.RemoveAll(work)
	var size int64
	if fi, err := os.Stat(final); err == nil {
		size = fi.Size()
	}
	audio, subs := trackLangs(inputs)
	if upgrade {
		setBlob(b.ID, `file_bytes=?, estimated_bytes=?, quality_label=?, height=?, duration_sec=?, audio_languages=?, subtitle_languages=?`,
			size, est, variantLabel(v), v.Height, dur, audio, subs)
	} else {
		setBlob(b.ID, `status=?, progress=1, file_bytes=?, completed_at=?, error='', audio_languages=?, subtitle_languages=?`,
			StatusCompleted, size, m.now().Unix(), audio, subs)
	}
	return nil
}

func trackLangs(inputs []ffInput) (audio, subs string) {
	var a, s []string
	for _, in := range inputs {
		label := trackLabel(TrackCriterion{Language: in.Language, Name: in.Name})
		switch in.Kind {
		case "audio":
			a = append(a, label)
		case "subtitle":
			s = append(s, label)
		}
	}
	return strings.Join(a, ","), strings.Join(s, ",")
}

// loadInput fetches an input's media playlist and writes its local copy.
func (m *Manager) loadInput(ctx context.Context, li *localInput, work string, headers map[string]string) error {
	text, err := m.fetchText(ctx, li.remoteURL, headers)
	if err != nil {
		return fmt.Errorf("playlist %s: %w", li.Kind, err)
	}
	mp := ParseMedia(text, li.remoteURL)
	switch {
	case mp.ByteRange:
		return ErrUnavailable{"formato della sorgente non supportato per il download (byte range)"}
	case !mp.Ended:
		return ErrUnavailable{"Le dirette non si possono scaricare"}
	case len(mp.Segments) == 0:
		return fmt.Errorf("playlist %s vuota", li.Kind)
	}
	local, res := mp.Localize(li.prefix, li.remoteURL)
	li.res = res
	li.URL = filepath.Join(work, li.prefix+".m3u8")
	return os.WriteFile(li.URL, []byte(local), 0o644)
}

// reconcileWork keeps a work dir only if it was made for the same inputs;
// otherwise (different choice matched, source re-encoded) it starts clean.
func reconcileWork(work string, locals []*localInput) error {
	var want workState
	for _, li := range locals {
		want.Inputs = append(want.Inputs, fmt.Sprintf("%s:%s:%d", li.Kind, li.Language, len(li.res)))
	}
	statePath := filepath.Join(work, "state.json")
	if raw, err := os.ReadFile(statePath); err == nil {
		var have workState
		if json.Unmarshal(raw, &have) == nil && strings.Join(have.Inputs, "|") != strings.Join(want.Inputs, "|") {
			log.Printf("[downloads] %s: la sorgente è cambiata, riparto da zero", filepath.Base(work))
			entries, _ := os.ReadDir(work)
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), ".m3u8") {
					_ = os.Remove(filepath.Join(work, e.Name()))
				}
			}
		}
	}
	raw, _ := json.Marshal(want)
	return os.WriteFile(statePath, raw, 0o644)
}

// fetchResource downloads one segment/key/init section, retrying; after a
// second failure it re-reads the input's playlist once or twice (fresh
// segment URLs: a token that expired while the job waited or paused).
func (m *Manager) fetchResource(ctx context.Context, li *localInput, ri int, work string, headers map[string]string) (int64, error) {
	var lastErr error
	// Few attempts: each already includes the proxy's own retry budget; a
	// missing segment goes to the job-level backoff.
	for attempt := 1; attempt <= 3; attempt++ {
		n, err := m.download(ctx, li.res[ri].URL, filepath.Join(work, li.res[ri].Name), headers)
		if err == nil {
			return n, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		log.Printf("[downloads] %s %s: tentativo %d/3 fallito: %v", li.Kind, li.res[ri].Name, attempt, err)
		lastErr = err
		if attempt >= 2 && li.refreshes < 2 {
			li.refreshes++
			if rerr := m.refreshInput(ctx, li, headers); rerr != nil {
				log.Printf("[downloads] rilettura playlist %s fallita: %v", li.Kind, rerr)
			}
		}
		select {
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, fmt.Errorf("%s %s: %w", li.Kind, li.res[ri].Name, lastErr)
}

// refreshInput re-reads the remote media playlist and takes its URLs, as long
// as it still has the same shape.
func (m *Manager) refreshInput(ctx context.Context, li *localInput, headers map[string]string) error {
	text, err := m.fetchText(ctx, li.remoteURL, headers)
	if err != nil {
		return err
	}
	_, res := ParseMedia(text, li.remoteURL).Localize(li.prefix, li.remoteURL)
	if len(res) != len(li.res) {
		return fmt.Errorf("la playlist è cambiata (%d → %d elementi)", len(li.res), len(res))
	}
	for i := range res {
		li.res[i].URL = res[i].URL
	}
	return nil
}

// download writes u to path (via a .tmp file, so a file that exists is whole).
func (m *Manager) download(ctx context.Context, u, path string, headers map[string]string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, throttle(ctx, resp.Body))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n == 0 {
		err = errors.New("risposta vuota")
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, os.Rename(tmp, path)
}

// pacer keeps a job's segment fetches at most factor× faster than the
// content plays (see paceFactor): it waits before a segment until the wall
// time since the first fetched segment is at least (content time)/factor.
// Segments already on disk (a resumed job) don't count.
type pacer struct {
	factor       float64
	start        time.Time
	contentStart float64 // playlist time where this run's fetching began
	lastEnd      float64
}

func (p *pacer) skip(r localResource) {
	if r.Kind == "segment" && r.End > 0 {
		p.lastEnd = r.End
	}
}

// delay is how long to wait before fetching r (0 = go now).
func (p *pacer) delay(r localResource, now time.Time) time.Duration {
	if p.factor <= 0 || r.Kind != "segment" || r.End <= 0 {
		return 0
	}
	if p.start.IsZero() {
		p.start, p.contentStart = now, p.lastEnd
		return 0
	}
	// This segment may start once the previous ones "played" at factor×.
	due := time.Duration((p.lastEnd - p.contentStart) / p.factor * float64(time.Second))
	if elapsed := now.Sub(p.start); elapsed < due {
		return due - elapsed
	}
	return 0
}

func (p *pacer) wait(ctx context.Context, r localResource) error {
	d := p.delay(r, time.Now())
	if r.Kind == "segment" && r.End > 0 {
		p.lastEnd = r.End
	}
	if d <= 0 {
		return nil
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// throttle paces reads to the max download speed setting (0 = unlimited).
func throttle(ctx context.Context, r io.Reader) io.Reader {
	rate := maxBytesPerSec()
	if rate <= 0 {
		return r
	}
	return &throttledReader{ctx: ctx, r: r, rate: rate, start: time.Now()}
}

type throttledReader struct {
	ctx   context.Context
	r     io.Reader
	rate  int64
	start time.Time
	read  int64
}

func (t *throttledReader) Read(p []byte) (int, error) {
	if len(p) > 32<<10 {
		p = p[:32<<10]
	}
	n, err := t.r.Read(p)
	t.read += int64(n)
	if due := time.Duration(float64(t.read) / float64(t.rate) * float64(time.Second)); due > time.Since(t.start) {
		select {
		case <-time.After(due - time.Since(t.start)):
		case <-t.ctx.Done():
			return n, t.ctx.Err()
		}
	}
	return n, err
}

// waitWhileStreaming holds the job between two segments while someone is
// watching something (setting on by default), then lets it go on.
func (m *Manager) waitWhileStreaming(ctx context.Context, blobID string) error {
	for pauseWhileStreaming() && streamingActive() {
		m.setPaused(blobID, true)
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.setPaused(blobID, false)
	return nil
}

// mux runs ffmpeg on the local playlists, deleting each segment once ffmpeg
// is well past it: segments and the growing output never both take full
// size on disk.
func (m *Manager) mux(ctx context.Context, b *blob, locals []*localInput, work, part string, dur float64, fetched int64, upgrade bool) error {
	inputs := make([]ffInput, len(locals))
	for i, li := range locals {
		inputs[i] = li.ffInput
	}
	args := buildFFmpegArgs(inputs, muxInputOpts, part, blobTitle(b.ID))
	cmd := exec.CommandContext(ctx, m.ffmpeg, args...)
	cmd.Dir = work
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("avvio ffmpeg: %w", err)
	}
	const keepBehind = 30.0 // seconds of segments kept behind ffmpeg's position
	progressTracker{onUpdate: func(outSec float64, size int64, end bool) {
		if !upgrade && dur > 0 {
			frac := fetchProgressShare + (1-fetchProgressShare)*min(1, outSec/dur)
			m.setProgress(b.ID, min(0.999, frac), fetched)
		}
		for _, li := range locals {
			for _, r := range li.res {
				if r.Kind == "segment" && r.End > 0 && r.End < outSec-keepBehind {
					_ = os.Remove(filepath.Join(work, r.Name))
				}
			}
		}
	}}.consume(stdout)
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := stderr.String()
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return fmt.Errorf("ffmpeg: %v: %s", err, msg)
	}
	return nil
}

// blobTitle is a title for the file's metadata, from any of its downloads.
func blobTitle(blobID string) string {
	var title, series string
	_ = managers.DB.QueryRow(`SELECT title, series_title FROM dl_items WHERE blob_id=? ORDER BY created_at LIMIT 1`, blobID).Scan(&title, &series)
	if series != "" {
		return strings.TrimSpace(series + " " + title)
	}
	return title
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func dirSize(dir string) int64 {
	var total int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && !e.IsDir() {
			total += fi.Size()
		}
	}
	return total
}
