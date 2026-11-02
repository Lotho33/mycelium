package api

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// webAppCompressible lists the extensions of a Flutter web build worth
// gzipping: main.dart.js and the CanvasKit/skwasm .wasm files shrink to
// roughly a third and dominate the first load.
var webAppCompressible = map[string]bool{
	".js": true, ".mjs": true, ".wasm": true, ".json": true, ".css": true,
	".svg": true, ".otf": true, ".ttf": true, ".txt": true, ".map": true,
}

// webAppGzEntry is one compressed file, valid while the source keeps the
// same mtime and size (a web-app update swaps the whole directory).
type webAppGzEntry struct {
	modTime time.Time
	size    int64
	data    []byte
}

var (
	webAppGzMu    sync.Mutex
	webAppGzCache = map[string]*webAppGzEntry{}
)

// acceptsGzip reports whether the request's Accept-Encoding allows gzip.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			continue
		}
		if v, found := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q="); found {
			q, err := strconv.ParseFloat(v, 64)
			return err != nil || q > 0
		}
		return true
	}
	return false
}

// webAppGzipped returns the gzipped content of path, compressing it on the
// first request and caching it in memory. ok is false when the file can't
// be read or compression doesn't make it smaller.
func webAppGzipped(path string, fi os.FileInfo) (data []byte, ok bool) {
	webAppGzMu.Lock()
	defer webAppGzMu.Unlock()
	if e := webAppGzCache[path]; e != nil && e.modTime.Equal(fi.ModTime()) && e.size == fi.Size() {
		return e.data, e.data != nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
	zw.Write(raw)
	zw.Close()
	e := &webAppGzEntry{modTime: fi.ModTime(), size: fi.Size()}
	if buf.Len() < len(raw) {
		e.data = buf.Bytes()
	}
	webAppGzCache[path] = e
	return e.data, e.data != nil
}

// serveWebAppFile serves one static file of a web build. File names aren't
// content-hashed (main.dart.js is main.dart.js in every release), so every
// response says no-cache: the browser keeps its copy but revalidates it
// (a cheap 304 via Last-Modified) and never runs a stale main.dart.js
// against a newer index.html after an update. Without any Cache-Control the
// browser applied heuristic freshness and could do exactly that.
func serveWebAppFile(w http.ResponseWriter, r *http.Request, path string) {
	w.Header().Set("Cache-Control", "no-cache")
	if !webAppCompressible[strings.ToLower(filepath.Ext(path))] {
		http.ServeFile(w, r, path)
		return
	}
	w.Header().Add("Vary", "Accept-Encoding")
	fi, err := os.Stat(path)
	if err != nil || !acceptsGzip(r) {
		http.ServeFile(w, r, path)
		return
	}
	data, ok := webAppGzipped(path, fi)
	if !ok {
		http.ServeFile(w, r, path)
		return
	}
	// A byte range of the compressed body means nothing to the client.
	r.Header.Del("Range")
	w.Header().Set("Content-Encoding", "gzip")
	// ServeContent picks the Content-Type from the name (.wasm included,
	// which WebAssembly.instantiateStreaming requires) and answers
	// If-Modified-Since with 304.
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), bytes.NewReader(data))
}
