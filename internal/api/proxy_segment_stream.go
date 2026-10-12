package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// Segments relayed as they arrive. A segment whose head is already clean
// media (TS or fMP4 at offset 0, plain audio/subtitles, ciphertext without
// a wrapper) needs no whole-body normalisation, so ProxySegment hands its
// bytes to the player while they come off the CDN instead of buffering it
// first. Buffering hid the upstream from the player: ExoPlayer measured the
// LAN instead of the CDN (adaptive bitrate never stepped down) and a slow
// CDN looked like a server that doesn't answer at all.
//
// What buffering gave is kept where it can be: a body cut mid-way is resumed
// with a Range request, a TS tail of junk is still trimmed (see tsTailGuard),
// and a leader's bytes still reach the warm cache for its player's retry.

// segmentHeadLen is how much of a body ProxySegment reads before choosing
// between streaming and buffering it (mediaTypeAtStart needs 4 TS packets).
const segmentHeadLen = 1024

// maxSegmentResumes bounds the Range requests that resume one cut body.
const maxSegmentResumes = 3

// readSegmentHead reads up to segmentHeadLen bytes. complete=true means the
// whole body fit in it.
func readSegmentHead(body io.Reader) (head []byte, complete bool, err error) {
	// Not io.ReadFull: it reports a clean short body and a connection cut
	// mid-body (net/http's io.ErrUnexpectedEOF) alike.
	head = make([]byte, segmentHeadLen)
	n := 0
	for n < len(head) {
		m, err := body.Read(head[n:])
		n += m
		if err == io.EOF {
			return head[:n], true, nil
		}
		if err != nil {
			return head[:n], false, err
		}
	}
	return head, false, nil
}

// streamableSegment reports whether a segment starting with head can be
// relayed as it arrives, and the Content-Type to relay it with. The rules
// mirror normalizeSegment's: only bodies it would pass through untouched
// (bar a TS tail trim) qualify.
func streamableSegment(head []byte, ct, rawURL string, encrypted bool) (string, bool) {
	if encrypted {
		// A wrapper in front of ciphertext is only confirmed once the whole body
		// is known (normalizeSegment): buffer those.
		if looksLikeTextPage(head) || hasImageSignature(head) {
			return "", false
		}
		return segmentTypeFromURL(rawURL), true
	}
	lct := strings.ToLower(strings.TrimSpace(ct))
	if mt := mediaTypeAtStart(head); mt != "" {
		if strings.HasPrefix(lct, "video/") {
			return ct, true
		}
		return mt, true
	}
	if other := nonVideoSegmentType(head); other != "" {
		if strings.HasPrefix(lct, "audio/") || strings.HasPrefix(lct, "text/vtt") {
			return ct, true
		}
		return other, true
	}
	return "", false
}

// hasImageSignature reports whether b starts like one of the images
// imageWrapperLen can parse (signature only: b may be a truncated head).
func hasImageSignature(b []byte) bool {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")),
		len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF,
		bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")),
		len(b) >= 2 && b[0] == 'B' && b[1] == 'M',
		len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return true
	}
	return false
}

// segStream is one segment being relayed as it arrives. Two sides: the
// producer reads upstream into a segPipe at the CDN's pace (and frees the
// CDN slot as soon as it is done), the handler writes the pipe to the
// player at the player's pace. A slow player (or a throttled download)
// never holds a CDN slot, and a leader whose player left still completes
// the segment for the warm cache.
type segStream struct {
	w        http.ResponseWriter
	r        *http.Request
	start    time.Time
	realURL  string
	resp     *http.Response // upstream response; its Body is read past head
	head     []byte
	ct       string
	encrypt  bool
	flight   *segFlight // nil for a follower fetching on its own
	sess     *managers.StreamSession
	release  func()          // frees the CDN slot (see segAcquire)
	ctx      context.Context // bounds resume requests (not the retry budget)
	client   *http.Client
	origin   string
	cookies  string
	xhdr     string
	resumeOK bool

	// Relaying a player's byte range (setRanged): the upstream status and
	// Content-Range go to the player as they came. rangeStart/rangeEnd are the
	// body's span in the upstream file (rangeEnd -1: to the end), for resume.
	status       int
	contentRange string
	rangeStart   int64
	rangeEnd     int64
}

// segmentBodyIdle is how long a streamed body may go without a byte before
// it is treated as cut (and resumed): the player's own read timeout is 20s.
// A var so tests can shrink it.
var segmentBodyIdle = 12 * time.Second

// setRanged configures s to relay resp, the upstream answer to a player's
// Range request, as is: no TS tail guard (the body may start or end
// mid-file), no warm cache.
func (s *segStream) setRanged(resp *http.Response) {
	s.status = resp.StatusCode
	s.ct = resp.Header.Get("Content-Type")
	if s.ct == "" {
		s.ct = segmentTypeFromURL(s.realURL)
	}
	if resp.StatusCode == http.StatusPartialContent {
		s.contentRange = resp.Header.Get("Content-Range")
		var total string
		if n, _ := fmt.Sscanf(s.contentRange, "bytes %d-%d/%s", &s.rangeStart, &s.rangeEnd, &total); n < 2 {
			// No usable span: a cut body can't be resumed.
			s.rangeStart, s.rangeEnd, s.resumeOK = 0, -1, false
		}
	}
}

// playerByteRange returns the Range header to forward upstream, "" for none
// (a request for the whole body, "bytes=0-", counts as none).
func playerByteRange(h string) string {
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(strings.ToLower(h), "bytes=") {
		return ""
	}
	if strings.ReplaceAll(strings.ToLower(h), " ", "") == "bytes=0-" {
		return ""
	}
	return h
}

// errSegAbandoned ends a producer whose bytes nobody can use any more.
var errSegAbandoned = errors.New("player gone, segment abandoned")

// segPipe carries a segment from the producer to its readers: the leader's
// player, plus any retry that joins while the segment is still arriving.
// While the segment fits in segmentBufferLimit it is all kept (it becomes
// the warm cache entry, and a reader can join from byte 0); past that,
// bytes every reader has had are dropped and the producer waits when it is
// segmentBufferLimit ahead of the slowest one.
type segPipe struct {
	mu   sync.Mutex
	cond *sync.Cond
	// buf holds bytes [base, base+len(buf)). Written bytes are never modified
	// (trimming allocates), so a reader may use a slice of it unlocked.
	buf      []byte
	base     int64
	readers  map[int]int64 // active reader → what it has been handed
	nextID   int
	keepAll  bool // buf still starts at byte 0 and fits the limit
	closed   bool
	err      error
	finished chan struct{}
}

// newSegPipe returns a pipe with one reader (id 0) already joined.
func newSegPipe() *segPipe {
	p := &segPipe{keepAll: true, readers: map[int]int64{0: 0}, nextID: 1, finished: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// minPosLocked is how far the slowest reader has got (the end when none).
func (p *segPipe) minPosLocked() int64 {
	m := p.base + int64(len(p.buf))
	for _, pos := range p.readers {
		m = min(m, pos)
	}
	return m
}

// Write appends b (never fails: the producer checks stop() itself).
func (p *segPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	if p.keepAll && len(p.buf) > segmentBufferLimit {
		p.keepAll = false
	}
	if !p.keepAll {
		// Too big to keep: drop what every reader has had, and wait while too
		// far ahead of the slowest.
		for {
			if k := p.minPosLocked() - p.base; k >= 1<<20 {
				p.buf = append([]byte(nil), p.buf[k:]...)
				p.base += k
			}
			if len(p.readers) == 0 || p.closed || p.base+int64(len(p.buf))-p.minPosLocked() <= segmentBufferLimit {
				break
			}
			p.cond.Wait()
		}
	}
	p.cond.Broadcast()
	return len(b), nil
}

// stop reports whether the producer should give up: every reader left and
// the bytes can't serve a retry either (no flight, or too big to cache).
func (p *segPipe) stop(leader bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.readers) == 0 && (!leader || !p.keepAll)
}

// close ends the segment (err == nil: complete).
func (p *segPipe) close(err error) {
	p.mu.Lock()
	p.closed, p.err = true, err
	p.cond.Broadcast()
	p.mu.Unlock()
	close(p.finished)
}

// join adds a reader starting at byte 0; false once the start is gone.
func (p *segPipe) join() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.base > 0 || (p.closed && p.err != nil) {
		return 0, false
	}
	id := p.nextID
	p.nextID++
	p.readers[id] = 0
	return id, true
}

// next waits for bytes from pos on for reader id, returning up to 64 KiB
// of them; end reports that the segment is over at pos+len(chunk) (err: how
// it ended).
func (p *segPipe) next(id int, pos int64) (chunk []byte, end bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readers[id] = pos
	p.cond.Broadcast() // room for a producer waiting on the limit
	for pos >= p.base+int64(len(p.buf)) && !p.closed {
		p.cond.Wait()
	}
	if avail := p.base + int64(len(p.buf)) - pos; avail > 0 {
		chunk = p.buf[pos-p.base : pos-p.base+min(avail, 64<<10)]
	}
	return chunk, p.closed && pos+int64(len(chunk)) == p.base+int64(len(p.buf)), p.err
}

// leave removes reader id.
func (p *segPipe) leave(id int) {
	p.mu.Lock()
	delete(p.readers, id)
	p.cond.Broadcast()
	p.mu.Unlock()
}

// servePipe writes reader id's view of pipe to the player (headers already
// sent). gone: the player left; err: the segment ended badly (only
// meaningful when !gone, and then the caller must break the connection).
func servePipe(w http.ResponseWriter, r *http.Request, pipe *segPipe, id int) (sent int64, gone bool, err error) {
	flusher, _ := w.(http.Flusher)
	for {
		chunk, end, perr := pipe.next(id, sent)
		if len(chunk) > 0 {
			if r.Context().Err() != nil {
				gone = true
				break
			}
			n, werr := w.Write(chunk)
			sent += int64(n)
			if werr != nil {
				gone = true
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if end {
			err = perr
			break
		}
	}
	pipe.leave(id)
	return sent, gone, err
}

// segHeaders sets the response headers of a streamed segment.
func segHeaders(w http.ResponseWriter, ct string, contentLength int64) {
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*")
	if contentLength > 0 {
		h.Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}
}

// serveJoined serves a retry from the leader's segment still arriving
// (see segFlight.stream). served=false: nothing was sent, the caller fetches
// on its own.
func serveJoined(w http.ResponseWriter, r *http.Request, st *segFlightStream, start time.Time, realURL string, sess *managers.StreamSession) (served bool) {
	id, ok := st.pipe.join()
	if !ok {
		return false
	}
	// Wait for the first bytes before committing to a 200.
	chunk, end, err := st.pipe.next(id, 0)
	if len(chunk) == 0 && end && err != nil {
		st.pipe.leave(id)
		return false
	}
	segHeaders(w, st.ct, st.contentLength)
	w.WriteHeader(http.StatusOK)
	log.Printf("[proxy/segment] joined the fetch under way after %s url=%s", time.Since(start).Round(time.Millisecond), logURL(realURL))
	sent, gone, err := servePipe(w, r, st.pipe, id)
	if err != nil && !gone {
		log.Printf("[proxy/segment] shared fetch failed after %d bytes url=%s: %v", sent, logURL(realURL), core.RedactURLError(err))
		panic(http.ErrAbortHandler)
	}
	if !gone && sess != nil {
		sess.RecordFetch()
	}
	return true
}

// tsTailGuard streams MPEG-TS packets and applies trimTSTail's rule on the
// fly: bytes after the first packet that doesn't start with the sync byte
// are held back; they are released as soon as the sync pattern resumes (a
// corrupt packet mid-way), and dropped at the end if it never does (junk
// appended to the segment, or a trailing partial packet).
type tsTailGuard struct {
	out     io.Writer
	pend    []byte
	held    bool // a bad packet was seen: hold everything after it
	free    bool // sync resumed after a bad packet: pass everything
	trimmed int
}

func (g *tsTailGuard) Write(p []byte) (int, error) {
	if g.free {
		return g.out.Write(p)
	}
	g.pend = append(g.pend, p...)
	if !g.held {
		n := 0
		for n+188 <= len(g.pend) && g.pend[n] == 0x47 {
			n += 188
		}
		if n < len(g.pend) && g.pend[n] != 0x47 {
			g.held = true
		}
		if n > 0 {
			g.out.Write(g.pend[:n])
			g.pend = append(g.pend[:0], g.pend[n:]...)
		}
	}
	// Wait for 4 packets' worth so tsSyncOffset checks as many syncs as it
	// would on the whole tail.
	if g.held && len(g.pend) >= 4*188 && tsSyncOffset(g.pend) >= 0 {
		g.free = true
		g.out.Write(g.pend)
		g.pend = nil
	}
	return len(p), nil
}

// finish handles the tail at the end of the body.
func (g *tsTailGuard) finish() {
	if len(g.pend) == 0 {
		return
	}
	if tsSyncOffset(g.pend) >= 0 {
		g.out.Write(g.pend)
	} else {
		g.trimmed = len(g.pend)
	}
	g.pend = nil
}

// run relays the segment and returns once the upstream side is done too.
func (s *segStream) run() {
	// Sniffed, not from the Content-Type: a TS tail may still be trimmed, so
	// no length is promised for those.
	isTS := !s.encrypt && s.status == 0 && mediaTypeAtStart(s.head) == "video/MP2T"
	var cl int64
	if !isTS && s.resp.ContentLength > 0 && !s.resp.Uncompressed {
		cl = s.resp.ContentLength
	}
	segHeaders(s.w, s.ct, cl)
	if ar := s.resp.Header.Get("Accept-Ranges"); ar != "" {
		s.w.Header().Set("Accept-Ranges", ar)
	}
	status := http.StatusOK
	if s.status != 0 {
		status = s.status
		if s.contentRange != "" {
			s.w.Header().Set("Content-Range", s.contentRange)
		}
	}
	s.w.WriteHeader(status)
	if proxyDebug {
		log.Printf("[proxy/segment] streaming as %s url=%s", s.ct, logURL(s.realURL))
	}

	pipe := newSegPipe()
	if s.flight != nil && s.status == 0 {
		// A retry arriving from now on reads along (serveJoined).
		s.flight.publish(&segFlightStream{pipe: pipe, ct: s.ct, contentLength: cl})
	}
	go s.produce(pipe, isTS)

	sent, gone, _ := servePipe(s.w, s.r, pipe, 0)
	<-pipe.finished
	err := pipe.err

	if err != nil {
		if !gone {
			log.Printf("[proxy/segment] upstream body read error after %d bytes url=%s: %v", sent, logURL(s.realURL), core.RedactURLError(err))
			// The player already has part of the body: break the connection so it
			// sees an error, not a short segment.
			panic(http.ErrAbortHandler)
		}
		return
	}
	if s.flight != nil && pipe.keepAll && (gone || s.flight.waiters.Load() > 0) {
		// Someone will ask for these bytes again: keep them for the retry.
		managers.Segments.Put(s.realURL, pipe.buf)
	}
	if gone {
		// Typically a seek or a quality switch (the resume seek right after the
		// stream opens drops segment 0), or the player's own retry: not an error.
		log.Printf("[proxy/segment] player closed the request after %s (%d of %d bytes sent) url=%s", time.Since(s.start).Round(time.Millisecond), sent, pipe.base+int64(len(pipe.buf)), logURL(s.realURL))
	}
	if s.sess != nil {
		s.sess.RecordFetch()
	}
}

// produce reads the upstream body into pipe, resuming a body cut mid-way,
// and closes the pipe with how it ended.
func (s *segStream) produce(pipe *segPipe, isTS bool) {
	body := s.resp.Body
	var out io.Writer = pipe
	var guard *tsTailGuard
	if isTS {
		guard = &tsTailGuard{out: pipe}
		out = guard
	}
	leader := s.flight != nil
	consumed := int64(len(s.head)) // upstream bytes read so far
	out.Write(s.head)

	// A body that stops sending is closed, so its Read fails and the cut is
	// resumed like any other.
	var bodyMu sync.Mutex
	var idleFired atomic.Bool
	idle := time.AfterFunc(segmentBodyIdle, func() {
		idleFired.Store(true)
		bodyMu.Lock()
		body.Close()
		bodyMu.Unlock()
	})
	defer idle.Stop()

	buf := make([]byte, 32<<10)
	resumes := 0
	var readErr error
	for {
		idle.Reset(segmentBodyIdle)
		n, err := body.Read(buf)
		if n > 0 {
			consumed += int64(n)
			out.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil && idleFired.Swap(false) {
			err = fmt.Errorf("no data for %s", segmentBodyIdle)
		}
		if pipe.stop(leader) {
			readErr = errSegAbandoned
			break
		}
		if err == nil {
			continue
		}
		// Body cut mid-way: carry on from where it stopped.
		if !s.resumeOK || resumes >= maxSegmentResumes || s.ctx.Err() != nil {
			readErr = err
			break
		}
		resumes++
		log.Printf("[proxy/segment] upstream body cut after %d bytes (%v), resuming (%d/%d) url=%s", consumed, core.RedactURLError(err), resumes, maxSegmentResumes, logURL(s.realURL))
		idle.Stop() // resume bounds its own wait for headers
		idleFired.Store(false)
		nb, rerr := s.resume(consumed)
		if rerr != nil {
			readErr = fmt.Errorf("resume: %w", rerr)
			break
		}
		bodyMu.Lock()
		old := body
		body = nb
		bodyMu.Unlock()
		old.Close()
	}
	idle.Stop()
	bodyMu.Lock()
	body.Close()
	bodyMu.Unlock()
	s.release()
	if guard != nil && readErr == nil {
		guard.finish()
		if guard.trimmed > 0 {
			log.Printf("[proxy/segment] trimmed %d trailing bytes after the last TS packet url=%s", guard.trimmed, logURL(s.realURL))
		}
	}
	pipe.close(readErr)
}

// resume asks upstream for the body from offset on, accepting only a 206
// that starts exactly there.
func (s *segStream) resume(offset int64) (io.ReadCloser, error) {
	req, err := buildProxyRequest(http.MethodGet, s.realURL, s.origin, s.cookies, s.xhdr)
	if err != nil {
		return nil, err
	}
	from := s.rangeStart + offset
	rg := "bytes=" + strconv.FormatInt(from, 10) + "-"
	if s.rangeEnd >= 0 {
		rg += strconv.FormatInt(s.rangeEnd, 10)
	}
	req.Header.Set("Range", rg)
	req.Header.Set(freshConnHeader, "1")
	ctx, cancel := context.WithCancel(s.ctx)
	timer := time.AfterFunc(segmentRetryAttemptTTFB, cancel)
	resp, err := s.client.Do(req.WithContext(ctx))
	if !timer.Stop() {
		if err == nil {
			resp.Body.Close()
		}
		cancel()
		return nil, fmt.Errorf("no response headers within %s", segmentRetryAttemptTTFB)
	}
	if err != nil {
		cancel()
		return nil, core.RedactURLError(err)
	}
	want := "bytes " + strconv.FormatInt(from, 10) + "-"
	if resp.StatusCode != http.StatusPartialContent || !strings.HasPrefix(resp.Header.Get("Content-Range"), want) || resp.Uncompressed {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("upstream answered %d (content-range %q) to a range request", resp.StatusCode, resp.Header.Get("Content-Range"))
	}
	return cancelOnClose{ReadCloser: resp.Body, cancel: cancel}, nil
}
