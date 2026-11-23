package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"mycelium/internal/engine"
)

// Image prewarm: the covers of the first items of each catalog are converted
// to the /img cache before a client asks, so the first home after a catalog
// refresh doesn't wait for (and the box doesn't spend a burst of CPU on)
// a screenful of conversions at once.
//
// The items come from the catalog counts the engine already fetches
// (engine.OnCatalogWarmed): no extra plugin calls. Only covers whose /img
// cache key is known in advance are prewarmed — the server-made versus
// covers and plain image URLs. TMDB images (clients fetch a size of their
// own, not through /img) and Jellyfin's (the client adds its own maxWidth)
// are left alone.
//
// It never competes with a client: one image at a time, and a conversion
// slot only when one is free right now (imgConvSem); otherwise it waits.

const (
	imgPrewarmPerCatalog = 8   // the first screenful of a carousel
	imgPrewarmQueue      = 128 // pending images; more are dropped
	imgPrewarmSlotWait   = 2 * time.Second
	imgPrewarmMaxWait    = 2 * time.Minute // then the image is skipped
	imgPrewarmTimeout    = 30 * time.Second
)

var (
	imgPrewarmOnce sync.Once
	imgPrewarmCh   chan string
	// imgPrewarmQueued dedups URLs already in the queue.
	imgPrewarmMu     sync.Mutex
	imgPrewarmQueued = map[string]bool{}
)

func init() {
	engine.OnCatalogWarmed = prewarmCatalogImages
}

// prewarmCatalogImages queues the covers of the first items of a catalog
// page (raw get_catalog result: an array, or {items: [...]}).
func prewarmCatalogImages(pluginID, catalogID string, raw []byte) {
	items := catalogItems(raw)
	if len(items) > imgPrewarmPerCatalog {
		items = items[:imgPrewarmPerCatalog]
	}
	for _, it := range items {
		for _, u := range []string{it.PosterURL, it.BackdropURL, it.ThumbnailURL, it.Extra["fanart_url"]} {
			if u != "" && prewarmable(u) {
				queuePrewarm(u)
			}
		}
	}
}

type prewarmItem struct {
	PosterURL    string            `json:"poster_url"`
	BackdropURL  string            `json:"backdrop_url"`
	ThumbnailURL string            `json:"thumbnail_url"`
	Extra        map[string]string `json:"extra"`
}

func catalogItems(raw []byte) []prewarmItem {
	var wrapper struct {
		Items []prewarmItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Items != nil {
		return wrapper.Items
	}
	var arr []prewarmItem
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	// extra with non-string values: decode without it.
	var loose []struct {
		PosterURL    string `json:"poster_url"`
		BackdropURL  string `json:"backdrop_url"`
		ThumbnailURL string `json:"thumbnail_url"`
	}
	if err := json.Unmarshal(raw, &loose); err == nil {
		out := make([]prewarmItem, len(loose))
		for i, l := range loose {
			out[i] = prewarmItem{PosterURL: l.PosterURL, BackdropURL: l.BackdropURL, ThumbnailURL: l.ThumbnailURL}
		}
		return out
	}
	return nil
}

var prewarmJellyfinRe = regexp.MustCompile(`(?i)/Items/[0-9a-f-]{32,36}/Images/`)

// prewarmable: the /img key of u is known before a client asks.
func prewarmable(u string) bool {
	lower := strings.ToLower(u)
	if strings.HasPrefix(lower, "mycelium:/img/versus?") {
		return true
	}
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return false
	}
	if strings.Contains(lower, ".svg") || strings.Contains(lower, "image.tmdb.org/") {
		return false
	}
	return !prewarmJellyfinRe.MatchString(u)
}

func queuePrewarm(u string) {
	imgPrewarmOnce.Do(func() {
		imgPrewarmCh = make(chan string, imgPrewarmQueue)
		go prewarmWorker()
	})
	imgPrewarmMu.Lock()
	defer imgPrewarmMu.Unlock()
	if imgPrewarmQueued[u] {
		return
	}
	select {
	case imgPrewarmCh <- u:
		imgPrewarmQueued[u] = true
	default: // queue full: the client will convert it, as before
	}
}

func prewarmWorker() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[img] prewarm: PANIC recuperato: %v", r)
		}
	}()
	done := 0
	for u := range imgPrewarmCh {
		if prewarmOne(u) {
			done++
		}
		imgPrewarmMu.Lock()
		delete(imgPrewarmQueued, u)
		empty := len(imgPrewarmCh) == 0
		imgPrewarmMu.Unlock()
		if empty && done > 0 {
			log.Printf("[img] prewarm: %d copertine pronte in cache", done)
			done = 0
		}
	}
}

// prewarmOne converts u into the cache unless it is there already or a
// client is converting it; true when it converted.
func prewarmOne(u string) bool {
	var cacheFile, home, away string
	if strings.HasPrefix(strings.ToLower(u), "mycelium:/img/versus?") {
		pu, err := url.Parse(u)
		if err != nil {
			return false
		}
		home, away = imgB64Decode(pu.Query().Get("h")), imgB64Decode(pu.Query().Get("a"))
		if home == "" || away == "" {
			return false
		}
		cacheFile = versusCacheFile(home, away)
	} else {
		cacheFile = imgCacheFile(u)
	}
	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		return false
	}

	// Same flight table as the handlers: a client converting it already wins.
	imgInflightMu.Lock()
	if _, busy := imgInflight[cacheFile]; busy {
		imgInflightMu.Unlock()
		return false
	}
	flight := &imgFlight{done: make(chan struct{})}
	imgInflight[cacheFile] = flight
	imgInflightMu.Unlock()
	defer func() {
		imgInflightMu.Lock()
		delete(imgInflight, cacheFile)
		imgInflightMu.Unlock()
		close(flight.done)
	}()

	// A free conversion slot right now, or wait: never queue behind clients.
	deadline := time.Now().Add(imgPrewarmMaxWait)
	for {
		select {
		case imgConvSem <- struct{}{}:
		default:
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(imgPrewarmSlotWait)
			continue
		}
		break
	}
	defer func() { <-imgConvSem }()

	ctx, cancel := context.WithTimeout(context.Background(), imgPrewarmTimeout)
	defer cancel()
	var out []byte
	if home != "" {
		h, err := imgFetchDecode(ctx, home)
		if err != nil {
			return false
		}
		a, err := imgFetchDecode(ctx, away)
		if err != nil {
			return false
		}
		if out, err = versusEncode(h, a); err != nil {
			return false
		}
	} else {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		var err error
		if out, _, _, _, _, err = imgFetchEncode(req, u); err != nil {
			return false
		}
	}
	if imgWriteCache(cacheFile, out) != nil {
		return false
	}
	flight.out = out
	return true
}
