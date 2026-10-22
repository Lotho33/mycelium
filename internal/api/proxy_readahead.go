package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// Read-ahead. A CDN that misses its cache can take 20-40s to fetch a segment
// from its origin, while a hit answers in a fraction of a second; most misses
// are fast, but the slow ones outlast a player's buffer. So while a VOD
// stream plays, each segment request also fetches the next
// readAheadSecs() worth of segments of the same playlist into the warm
// cache: a slow miss then happens well before the player gets there.
//
// Followers find the bytes through the usual paths: a read-ahead fetch is a
// segFlight leader (a player asking meanwhile waits for it), and its result
// lands in managers.Segments.

// raSeg is one segment of a registered playlist.
type raSeg struct {
	url string
	dur float64 // #EXTINF seconds (0 = unknown)
	enc bool    // ciphertext (after an active #EXT-X-KEY)
}

// raList is a VOD media playlist known to the read-ahead, with what its
// upstream requests need.
type raList struct {
	segs       []raSeg
	origin     string // decoded Referer/Origin
	cookiesRaw string
	xhdrRaw    string
	useVPN     bool
	egr        string
	inflight   atomic.Int32
}

type raPos struct {
	list *raList
	idx  int
}

var (
	raMu    sync.Mutex
	raByURL = map[string]raPos{} // segment URL → its place in a list
	raLists []*raList            // oldest first, for pruning
)

// raMaxLists bounds how many playlists are remembered (a few per stream:
// video and audio renditions).
const raMaxLists = 32

// raPerList caps the read-ahead fetches running for one playlist.
const raPerList = 2

// readAheadTimeout bounds one read-ahead fetch: long enough for a slow CDN
// miss, which is the whole point.
var readAheadTimeout = 60 * time.Second

// readAheadSlow is when a read-ahead fetch is worth a log line.
const readAheadSlow = 5 * time.Second

// readAheadSecs is how far ahead (in media seconds) segments are fetched:
// setting prebuffer_readahead_secs or env MYCELIUM_PREBUFFER_READAHEAD_SECS,
// default 45; 0 turns it off.
func readAheadSecs() float64 {
	if v := strings.TrimSpace(os.Getenv("MYCELIUM_PREBUFFER_READAHEAD_SECS")); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	switch v := managers.Settings.Get("prebuffer_readahead_secs", nil).(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return n
		}
	}
	return 45
}

// readAheadRegister remembers a playlist's segments. A playlist registered
// again (a reload) replaces the entries of its segments.
func readAheadRegister(l *raList) {
	if len(l.segs) == 0 {
		return
	}
	raMu.Lock()
	defer raMu.Unlock()
	raLists = append(raLists, l)
	for i, s := range l.segs {
		raByURL[s.url] = raPos{list: l, idx: i}
	}
	for len(raLists) > raMaxLists {
		old := raLists[0]
		raLists = raLists[1:]
		for _, s := range old.segs {
			if p, ok := raByURL[s.url]; ok && p.list == old {
				delete(raByURL, s.url)
			}
		}
	}
}

// raGlobal caps read-ahead fetches in flight across every playlist: each may
// buffer up to segmentBufferLimit.
var raGlobal = make(chan struct{}, 4)

// readAheadFrom starts fetching what follows segURL in its playlist, up to
// readAheadSecs() of media, skipping what is cached or already being fetched.
func readAheadFrom(segURL string) {
	window := readAheadSecs()
	if window <= 0 {
		return
	}
	raMu.Lock()
	pos, ok := raByURL[segURL]
	raMu.Unlock()
	if !ok {
		return
	}
	l := pos.list
	ahead := 0.0
	for j := pos.idx + 1; j < len(l.segs) && ahead < window; j++ {
		s := l.segs[j]
		if s.dur > 0 {
			ahead += s.dur
		} else {
			ahead += 6
		}
		if managers.Segments.Has(s.url) {
			continue
		}
		if _, busy := segFlights.Load(s.url); busy {
			continue
		}
		if l.inflight.Add(1) > raPerList {
			l.inflight.Add(-1)
			return
		}
		select {
		case raGlobal <- struct{}{}:
		default:
			l.inflight.Add(-1)
			return // enough read-ahead in flight server-wide
		}
		core.SafeGo("proxy/readahead", func() {
			defer func() { <-raGlobal }()
			defer l.inflight.Add(-1)
			readAheadFetch(l, s)
		})
	}
}

// readAheadFetch fetches one segment into the warm cache, as a segFlight
// leader so a player asking meanwhile waits for it instead of fetching again.
func readAheadFetch(l *raList, s raSeg) {
	f, leader := joinSegFlight(s.url)
	if !leader {
		f.waiters.Add(-1)
		return
	}
	defer f.finish(s.url)
	if managers.Segments.Has(s.url) {
		return
	}
	client, err := upstreamSegmentClient(l.useVPN, l.egr)
	if err != nil {
		return
	}
	req, err := buildProxyRequest(http.MethodGet, s.url, l.origin, l.cookiesRaw, l.xhdrRaw)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), readAheadTimeout)
	defer cancel()
	// Same per-host CDN budget as the players' own fetches.
	release, err := segAcquire(ctx, req.URL.Host)
	if err != nil {
		return
	}
	defer release()
	start := time.Now()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		log.Printf("[proxy/readahead] fetch failed after %s url=%s: %v", time.Since(start).Round(time.Millisecond), logURL(s.url), core.RedactURLError(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[proxy/readahead] upstream %d url=%s", resp.StatusCode, logURL(s.url))
		return
	}
	ttfb := time.Since(start)
	body, err := io.ReadAll(io.LimitReader(resp.Body, segmentBufferLimit+1))
	if err != nil || len(body) > segmentBufferLimit {
		return
	}
	v := normalizeSegment(body, resp.Header.Get("Content-Type"), s.url, s.enc)
	if v.reject != "" {
		return
	}
	managers.Segments.Put(s.url, v.body)
	if ttfb >= readAheadSlow {
		log.Printf("[proxy/readahead] slow upstream answer (%s) absorbed ahead of the player url=%s", ttfb.Round(time.Millisecond), logURL(s.url))
	} else if proxyDebug {
		log.Printf("[proxy/readahead] cached in %s url=%s", ttfb.Round(time.Millisecond), logURL(s.url))
	}
}
