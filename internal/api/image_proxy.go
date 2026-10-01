package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	stddraw "image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	webp "github.com/gen2brain/webp"
	xdraw "golang.org/x/image/draw"

	"mycelium/internal/core"
)

// ─────────────────────────────────────────────────────────────────────────────
// Image proxy — GET /img?u=<base64url upstream URL>
// ─────────────────────────────────────────────────────────────────────────────
//
// Fetches an upstream image once, re-encodes it as WebP at the source's own
// resolution and caches it on disk; the client picks the display size.
// imgMaxDimension is only a guard against pathological sources. Conversion
// is lazy (on first request). On failure it serves the original bytes when
// they are a raster image, else redirects to the original URL.

const (
	imgWebPQuality  = 82   // WebP q82 ≈ JPEG q90, smaller
	imgMaxDimension = 2000 // safety cap only
	imgFetchTimeout = 15 * time.Second
	imgSrcReadLimit = 25 << 20 // refuse larger upstreams
	// imgMaxSourcePixels caps the declared source size (image.DecodeConfig, before
	// decoding): a tiny PNG can declare a canvas that takes gigabytes to decode.
	imgMaxSourcePixels = 30_000_000
	imgCacheMaxBytes   = 300 << 20 // then oldest-first sweep
)

// imgDebug enables the per-request cache hit/store lines.
var imgDebug = os.Getenv("MYCELIUM_DEBUG") == "1"

// imgClient fetches upstream images through the SSRF-guarded dialer (/img
// URLs come from clients and aren't signed). LAN addresses are allowed
// unless MYCELIUM_PROXY_BLOCK_PRIVATE=1.
var imgClient = &http.Client{
	Timeout: imgFetchTimeout,
	Transport: &http.Transport{
		DialContext:         core.GuardedDialContext(nil),
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	},
}

// init registers a WebP decoder (the standard library has none), so WebP
// sources go through the normal re-encode/cache path.
func init() {
	image.RegisterFormat("webp", "RIFF????WEBP",
		func(r io.Reader) (image.Image, error) { return webp.Decode(r) },
		func(r io.Reader) (image.Config, error) { return webp.DecodeConfig(r) },
	)
}

var (
	imgCacheDirOnce sync.Once
	imgCacheDir     string
	imgSweepMu      sync.Mutex
	imgLastSweep    time.Time
)

func imageCacheDir() string {
	imgCacheDirOnce.Do(func() {
		imgCacheDir = core.AppPath("data", "imgcache")
		_ = os.MkdirAll(imgCacheDir, 0o755)
	})
	return imgCacheDir
}

// ImageProxy serves an upstream image re-encoded as WebP (see the file
// comment). Unauthenticated: u is a base64url URL.
func ImageProxy(w http.ResponseWriter, r *http.Request) {
	upstream := imgB64Decode(r.URL.Query().Get("u"))
	if upstream == "" || !strings.HasPrefix(upstream, "http") {
		http.Error(w, "bad u", http.StatusBadRequest)
		return
	}

	// SVG is not re-encoded: redirect to it.
	if strings.Contains(strings.ToLower(upstream), ".svg") {
		http.Redirect(w, r, upstream, http.StatusFound)
		return
	}

	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|q%d", upstream, imgWebPQuality)))
	cacheFile := filepath.Join(imageCacheDir(), hex.EncodeToString(sum[:])+".webp")

	if fi, err := os.Stat(cacheFile); err == nil && fi.Size() > 0 {
		now := time.Now()
		_ = os.Chtimes(cacheFile, now, now) // keep hot entries out of the sweep
		if imgDebug {
			log.Printf("[img] cache hit %s (%dKB)", imgShortURL(upstream), fi.Size()/1024)
		}
		imgServeHeaders(w)
		http.ServeFile(w, r, cacheFile)
		return
	}

	out, srcLen, srcFmt, raw, _, err := imgFetchEncode(r, upstream)
	if err != nil {
		if raw != nil {
			// Fetched but not re-encodable (animated, AVIF…): serve the bytes ourselves,
			// same-origin, rather than redirect — browsers apply CORS to the redirect.
			// Only bytes that sniff as a raster image, under the sniffed type: /img
			// shares the dashboard's origin, so an upstream HTML body must never be
			// echoed.
			if ct := imgSniffRasterType(raw); ct != "" {
				log.Printf("[img] passthrough-inline (%v): %s", err, imgShortURL(upstream))
				imgSecurityHeaders(w)
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
				_, _ = w.Write(raw)
				return
			}
		}
		// The fetch itself failed: redirect to the original.
		log.Printf("[img] passthrough (%v): %s", err, imgShortURL(upstream))
		http.Redirect(w, r, upstream, http.StatusFound) // fail open
		return
	}

	// tmp + rename so a concurrent read never sees a half-written file.
	note := ""
	tmp := cacheFile + ".tmp" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if e := os.WriteFile(tmp, out, 0o644); e == nil {
		_ = os.Rename(tmp, cacheFile)
		core.SafeGo("api/img-cache-sweep", imgCacheSweep)
	} else {
		_ = os.Remove(tmp)
		note = " (cache write failed)"
	}

	log.Printf("[img] converted %s  %dKB %s -> %dKB webp%s",
		imgShortURL(upstream), srcLen/1024, srcFmt, len(out)/1024, note)

	imgServeHeaders(w)
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	_, _ = w.Write(out)
}

// imgShortURL trims the query and caps the length of a URL for logs.
func imgShortURL(u string) string {
	if p, err := url.Parse(u); err == nil && p.Path != "" {
		u = p.Host + p.Path
	}
	if len(u) > 96 {
		u = u[:93] + "..."
	}
	return u
}

// imgSecurityHeaders keeps whatever /img serves inert on this origin: no
// MIME sniffing and a sandbox CSP.
func imgSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
}

// imgSniffRasterType returns raw's content type when it is a raster image
// (never SVG, which can carry script), else "".
func imgSniffRasterType(raw []byte) string {
	// AVIF: an ISO-BMFF "ftyp" box with an avif/avis brand.
	if len(raw) >= 12 && string(raw[4:8]) == "ftyp" {
		if brand := string(raw[8:12]); brand == "avif" || brand == "avis" {
			return "image/avif"
		}
	}
	ct := http.DetectContentType(raw)
	if strings.HasPrefix(ct, "image/") && !strings.Contains(ct, "svg") {
		return ct
	}
	return ""
}

func imgServeHeaders(w http.ResponseWriter) {
	imgSecurityHeaders(w)
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// imgFetchEncode downloads upstream, normalises it to NRGBA (keeping alpha),
// applies the size cap and encodes WebP; it also returns the source length
// and format for logging. rawBody/rawContentType are set only when the fetch
// succeeded but re-encoding failed, so the caller can serve the bytes.
func imgFetchEncode(r *http.Request, upstream string) (out []byte, srcLen int, srcFmt string, rawBody []byte, rawContentType string, err error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, nil)
	if err != nil {
		return nil, 0, "", nil, "", err
	}
	req.Header.Set("User-Agent", proxyUserAgent)
	req.Header.Set("Accept", "image/webp,image/jpeg,image/png,*/*")

	resp, err := imgClient.Do(req)
	if err != nil {
		return nil, 0, "", nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, "", nil, "", fmt.Errorf("upstream %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, imgSrcReadLimit))
	if err != nil {
		return nil, 0, "", nil, "", err
	}
	srcLen = len(raw)
	rawContentType = resp.Header.Get("Content-Type")

	// Check the declared dimensions first (see imgMaxSourcePixels).
	if cfg, _, cerr := image.DecodeConfig(bytes.NewReader(raw)); cerr == nil {
		if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > imgMaxSourcePixels {
			return nil, srcLen, "", nil, "", fmt.Errorf("source too large: %dx%d", cfg.Width, cfg.Height)
		}
	}

	src, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, srcLen, "", raw, rawContentType, err // unknown/animated format: caller serves raw
	}
	srcFmt = format

	dst := imgNormalize(src)

	var b bytes.Buffer
	// Method 4: the library's balanced speed/size setting.
	if err := webp.Encode(&b, dst, webp.Options{Quality: imgWebPQuality, Method: 4}); err != nil {
		return nil, srcLen, srcFmt, raw, rawContentType, err
	}
	return b.Bytes(), srcLen, srcFmt, nil, "", nil
}

// imgNormalize converts src to *image.NRGBA, scaling it down
// (aspect-preserving, Catmull-Rom) only beyond imgMaxDimension.
func imgNormalize(src image.Image) *image.NRGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 {
		return image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}

	dw, dh := sw, sh
	if sw > imgMaxDimension || sh > imgMaxDimension {
		if sw >= sh {
			dw = imgMaxDimension
			dh = sh * imgMaxDimension / sw
		} else {
			dh = imgMaxDimension
			dw = sw * imgMaxDimension / sh
		}
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
	}

	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	if dw == sw && dh == sh {
		stddraw.Draw(dst, dst.Bounds(), src, sb.Min, stddraw.Src)
	} else {
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, xdraw.Src, nil)
	}
	return dst
}

func imgB64Decode(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, " ", "+")
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b)
		}
	}
	return ""
}

// imgCacheSweep deletes the oldest files once the cache exceeds
// imgCacheMaxBytes (at most once a minute).
func imgCacheSweep() {
	imgSweepMu.Lock()
	defer imgSweepMu.Unlock()
	if time.Since(imgLastSweep) < time.Minute {
		return
	}
	imgLastSweep = time.Now()

	dir := imageCacheDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type ent struct {
		path string
		size int64
		mod  time.Time
	}
	list := make([]ent, 0, len(entries))
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, ent{filepath.Join(dir, e.Name()), fi.Size(), fi.ModTime()})
		total += fi.Size()
	}
	if total <= imgCacheMaxBytes {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.Before(list[j].mod) })
	for _, e := range list {
		if total <= imgCacheMaxBytes {
			break
		}
		if os.Remove(e.path) == nil {
			total -= e.size
		}
	}
}
