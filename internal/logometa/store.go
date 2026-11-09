package logometa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	webp "github.com/gen2brain/webp"

	"mycelium/internal/core"
)

// Measured logos are kept in memory and saved to data/logo_meta.json. A URL
// nobody has measured yet is queued and measured by a couple of background
// workers; Lookup never blocks on the network or the disk (beyond the first
// load of the file).

const (
	okTTL      = 30 * 24 * time.Hour
	failTTL    = 6 * time.Hour // fetch/decode failed: try again later
	unsupTTL   = 30 * 24 * time.Hour
	maxEntries = 20000

	queueSize    = 512
	workers      = 2
	fetchTimeout = 15 * time.Second
	// maxSourceBytes / maxSourcePixels: a logo bigger than this is refused
	// (the pixel cap is checked on the declared size, before decoding).
	maxSourceBytes  = 8 << 20
	maxSourcePixels = 16_000_000

	saveEvery = 30 * time.Second
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

type entry struct {
	Meta
	At   int64  `json:"at"`             // unix seconds of the measurement
	Fail string `json:"fail,omitempty"` // why it could not be measured
}

func (e *entry) expired(now time.Time) bool {
	ttl := okTTL
	switch {
	case e.Fail == errUnsupported.Error():
		ttl = unsupTTL
	case e.Fail != "":
		ttl = failTTL
	}
	return now.Sub(time.Unix(e.At, 0)) > ttl
}

var errUnsupported = errors.New("unsupported format")

var (
	mu      sync.RWMutex
	entries map[string]*entry
	pending = map[string]bool{}
	dirty   bool

	startOnce sync.Once
	queue     chan string

	// Overridable in tests.
	// storePath "" = memory only (tests never touch data/).
	storePath = func() string {
		if testing.Testing() {
			return ""
		}
		return core.AppPath("data", "logo_meta.json")
	}
	fetch = fetchImage
	now   = time.Now
)

func init() {
	// The standard library has no WebP decoder (image_proxy registers the
	// same one; the first match wins, either is fine).
	image.RegisterFormat("webp", "RIFF????WEBP",
		func(r io.Reader) (image.Image, error) { return webp.Decode(r) },
		func(r io.Reader) (image.Config, error) { return webp.DecodeConfig(r) },
	)
}

// Lookup returns the measurements of the logo at raw (a logo_url as plugins
// return it). When there are none yet, it queues the URL and returns false:
// a later call finds them.
func Lookup(raw string) (Meta, bool) {
	u := SourceURL(raw)
	if u == "" {
		return Meta{}, false
	}
	start()
	mu.RLock()
	e := entries[u]
	mu.RUnlock()
	if e != nil && !e.expired(now()) {
		return e.Meta, e.Fail == ""
	}
	enqueue(u)
	return Meta{}, false
}

func start() {
	startOnce.Do(func() {
		load()
		queue = make(chan string, queueSize)
		for i := 0; i < workers; i++ {
			core.SafeGo("logometa/worker", worker)
		}
		core.SafeGo("logometa/saver", saver)
	})
}

func enqueue(u string) {
	mu.Lock()
	if pending[u] {
		mu.Unlock()
		return
	}
	pending[u] = true
	mu.Unlock()
	select {
	case queue <- u:
	default:
		// Full: drop it, the next response asking for it queues it again.
		mu.Lock()
		delete(pending, u)
		mu.Unlock()
	}
}

func worker() {
	for u := range queue {
		measure(u)
	}
}

func measure(u string) {
	e := &entry{At: now().Unix()}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	img, err := fetch(ctx, u)
	cancel()
	if err != nil {
		e.Fail = err.Error()
		if !errors.Is(err, errUnsupported) {
			log.Printf("[logometa] %s: %v", shortURL(u), core.RedactURLError(err))
		}
	} else {
		e.Meta = Analyze(img)
	}
	mu.Lock()
	entries[u] = e
	delete(pending, u)
	dirty = true
	if len(entries) > maxEntries {
		evictOldest()
	}
	mu.Unlock()
}

// evictOldest drops the oldest tenth of the entries. mu held.
func evictOldest() {
	type kv struct {
		k  string
		at int64
	}
	all := make([]kv, 0, len(entries))
	for k, e := range entries {
		all = append(all, kv{k, e.At})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	for _, x := range all[:len(all)/10+1] {
		delete(entries, x.k)
	}
}

// ── source URL ──────────────────────────────────────────────────────────────

// legacySuffix matches the "|norm_w|norm_h|dark" some plugins append to
// logo_url (from mycelium.image.analyze_logo).
var legacySuffix = regexp.MustCompile(`\|\d+\|\d+\|[01]$`)

// tmdbImage matches TMDB image URLs: /t/p/<size>/<file>.<ext>.
var tmdbImage = regexp.MustCompile(`^(https?://image\.tmdb\.org/t/p/)[a-z0-9_]+(/[A-Za-z0-9_-]+)\.(svg|png|jpe?g|webp)$`)

// SourceURL returns the URL measured for a logo_url, or "" when there is
// nothing to measure. A legacy "|w|h|dark" suffix is dropped; TMDB logos are
// measured on their 300 px PNG (TMDB serves one for SVG logos too, and the
// statistics don't need more).
func SourceURL(raw string) string {
	u := strings.TrimSpace(legacySuffix.ReplaceAllString(strings.TrimSpace(raw), ""))
	if u == "" || len(u) > 2048 {
		return ""
	}
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
		return ""
	}
	if m := tmdbImage.FindStringSubmatch(u); m != nil {
		ext := m[3]
		if ext == "svg" {
			ext = "png"
		}
		return m[1] + "w300" + m[2] + "." + ext
	}
	return u
}

// ── fetch ───────────────────────────────────────────────────────────────────

var client = &http.Client{
	Timeout: fetchTimeout,
	Transport: &http.Transport{
		DialContext:         core.GuardedDialContext(nil),
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

func fetchImage(ctx context.Context, u string) (image.Image, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/webp,image/png,image/jpeg,*/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSourceBytes {
		return nil, errors.New("image over 8 MB")
	}
	return decode(raw)
}

func decode(raw []byte) (image.Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		// SVG, AVIF, HTML…: nothing this package can measure.
		return nil, errUnsupported
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxSourcePixels {
		return nil, fmt.Errorf("image too large: %dx%d", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return img, nil
}

func shortURL(u string) string {
	if p, err := url.Parse(u); err == nil {
		u = p.Host + p.Path
	}
	if len(u) > 96 {
		u = u[:93] + "..."
	}
	return u
}

// ── persistence ─────────────────────────────────────────────────────────────

func load() {
	mu.Lock()
	defer mu.Unlock()
	entries = map[string]*entry{}
	if storePath() == "" {
		return
	}
	data, err := os.ReadFile(storePath())
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &entries); err != nil || entries == nil {
		log.Printf("[logometa] %s unreadable, starting empty: %v", storePath(), err)
		entries = map[string]*entry{}
	}
}

func saver() {
	t := time.NewTicker(saveEvery)
	defer t.Stop()
	for range t.C {
		save()
	}
}

// save writes the entries when they changed (tmp + rename).
func save() {
	mu.Lock()
	if !dirty {
		mu.Unlock()
		return
	}
	data, err := json.Marshal(entries)
	dirty = false
	mu.Unlock()
	if err != nil {
		return
	}
	p := storePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil || os.Rename(tmp, p) != nil {
		_ = os.Remove(tmp)
		mu.Lock()
		dirty = true
		mu.Unlock()
	}
}

// Forget drops every measurement (factory reset / wipe).
func Forget() {
	start()
	mu.Lock()
	entries = map[string]*entry{}
	dirty = true
	mu.Unlock()
	if p := storePath(); p != "" {
		_ = os.Remove(p)
	}
}

// Put records m for the logo at raw as if it had just been measured (for
// tests of the packages that read the measurements).
func Put(raw string, m Meta) {
	u := SourceURL(raw)
	if u == "" {
		return
	}
	start()
	mu.Lock()
	entries[u] = &entry{Meta: m, At: now().Unix()}
	mu.Unlock()
}
