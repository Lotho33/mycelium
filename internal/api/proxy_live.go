package api

import (
	"bytes"
	"regexp"
	"strconv"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// Live streams shared between players. Players of one live stream on the
// same upstream playlist (ResolveStream hands them the same link when the
// plugin allows it) are served from a single upstream flow:
//   - the media playlist is fetched upstream at most once per half target
//     duration, whoever asks;
//   - each upstream fetch of it pulls the newest segments into the warm
//     cache, so every player finds them in RAM.
// It runs on the players' own reloads: nobody watching, nothing fetched.

// liveTailSegs is how many of the newest segments are pulled per reload: a
// player starts about three segments from the live edge.
const liveTailSegs = 3

// liveSegTTL keeps a live segment in RAM: players a few seconds apart all
// get it, and nobody asks once the live edge has moved on.
const liveSegTTL = 60 * time.Second

var reTargetDuration = regexp.MustCompile(`#EXT-X-TARGETDURATION:\s*(\d+)`)

type livePlaylist struct {
	body  []byte
	until time.Time
}

var livePlaylists = struct {
	sync.Mutex
	m map[string]livePlaylist
}{m: map[string]livePlaylist{}}

// isLiveMediaPlaylist: segments and no end tag (a sliding window).
func isLiveMediaPlaylist(b []byte) bool {
	return bytes.Contains(b, []byte("#EXTINF")) && !bytes.Contains(b, []byte("#EXT-X-ENDLIST"))
}

// livePlaylistTTL is half the target duration, between 1 and 3 seconds: the
// HLS spec lets a player see the same playlist again and retry sooner.
func livePlaylistTTL(b []byte) time.Duration {
	ttl := time.Second
	if m := reTargetDuration.FindSubmatch(b); m != nil {
		if td, err := strconv.Atoi(string(m[1])); err == nil {
			ttl = time.Duration(td) * time.Second / 2
		}
	}
	return min(max(ttl, time.Second), 3*time.Second)
}

func livePlaylistGet(url string) ([]byte, bool) {
	livePlaylists.Lock()
	defer livePlaylists.Unlock()
	e, ok := livePlaylists.m[url]
	if !ok || time.Now().After(e.until) {
		return nil, false
	}
	return e.body, true
}

func livePlaylistPut(url string, body []byte) {
	now := time.Now()
	livePlaylists.Lock()
	defer livePlaylists.Unlock()
	for k, e := range livePlaylists.m {
		if now.After(e.until) {
			delete(livePlaylists.m, k)
		}
	}
	livePlaylists.m[url] = livePlaylist{body: body, until: now.Add(livePlaylistTTL(body))}
}

// liveGlobal caps the live segment fetches in flight across every stream.
var liveGlobal = make(chan struct{}, 6)

// liveBufferTail fetches the newest segments of a live playlist into the
// warm cache, skipping what is cached or already being fetched.
func liveBufferTail(l *raList) {
	segs := l.segs
	if len(segs) > liveTailSegs {
		segs = segs[len(segs)-liveTailSegs:]
	}
	for _, s := range segs {
		if managers.Segments.Has(s.url) {
			continue
		}
		if _, busy := segFlights.Load(s.url); busy {
			continue
		}
		select {
		case liveGlobal <- struct{}{}:
		default:
			return
		}
		core.SafeGo("proxy/live-buffer", func() {
			defer func() { <-liveGlobal }()
			readAheadFetch(l, s)
		})
	}
}
