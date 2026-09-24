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
// Image proxy  —  GET /img?u=<base64url upstream>
// ─────────────────────────────────────────────────────────────────────────────
//
// Pileus routes poster / fanart / backdrop image URLs through here: mycelium
// fetches the upstream image once, re-encodes it as WebP at the SAME
// resolution as the source, and disk-caches the result. It does NOT resize —
// picking the display resolution (and any decode-time downscale) is the
// client's job. The single re-encode is enough of a win on its own: WebP is
// materially smaller than the upstream JPEG/PNG at equal quality, and because
// the cache key no longer carries a width every device/profile that asks for
// the same image shares one cached file.
//
// A hard dimension cap (imgMaxDimension) still applies, but only as a guard
// against a pathological upstream handing a client a giant image to decode —
// it is a single fixed value, not a per-request size, so it never fragments
// the cache.
//
// Conversion is lazy: an image is fetched + converted only when a client
// actually requests it. Nothing walks a plugin catalog to pre-convert.
//
// Fails open: on ANY problem (bad params, upstream error, unknown/animated
// format, decode failure) it 302-redirects to the original URL, so a client
// pointed here always still gets its image — including when it's talking to
// an older mycelium without this route (that 404s and the client's
// CachedNetworkImage shows its error placeholder; pileus only rewrites URLs
// when it expects this endpoint to exist).
//
// `?w=` is still accepted from older clients and simply ignored.

const (
	imgWebPQuality  = 82               // WebP scale is not 1:1 with JPEG; ~80-85 ≈ JPEG q90 at a smaller size
	imgMaxDimension = 2000             // safety cap only — downscale a pathologically large source to fit
	imgFetchTimeout = 15 * time.Second //
	imgSrcReadLimit = 25 << 20         // 25 MB — refuse pathological upstreams
	// imgMaxSourcePixels caps the DECLARED source size, checked with
	// image.DecodeConfig before any full decode: a few-KB PNG can declare
	// 40000×40000 and make image.Decode allocate ~6 GB (decompression bomb),
	// OOM-killing the container. 30 MP (≈120 MB as NRGBA) is well above any
	// real poster/backdrop.
	imgMaxSourcePixels = 30_000_000
	imgCacheMaxBytes   = 300 << 20 // ~300 MB of images, then oldest-first sweep
)

// imgDebug gates the per-request hit/store lines. The conversion and
// passthrough lines below are always logged: a conversion fires once per
// unique image (then it's cached forever), and a passthrough means the
// feature silently fell back — both are worth seeing without a flag.
var imgDebug = os.Getenv("MYCELIUM_DEBUG") == "1"

// imgClient fetches upstream images. Its dialer carries the SSRF guard: /img
// URLs are minted client-side and can't be signed, so this is their only
// protection. Loopback / link-local / cloud-metadata targets are refused;
// LAN (RFC1918) posters keep working unless MYCELIUM_PROXY_BLOCK_PRIVATE=1.
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

// init registers gen2brain/webp as a source decoder so an upstream that
// already serves WebP goes through the re-encode/cache path instead of the
// fail-open redirect. The stdlib image package has no WebP decoder. Wrapped
// because webp.Decode/DecodeConfig take variadic options and don't match
// image.RegisterFormat's signature directly; a redundant registration (if the
// package ever self-registers) is harmless.
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

// ImageProxy godoc
//
//	@Summary		Poster/fanart image (WebP)
//	@Description	Re-encode an upstream image as WebP (no resize — the client picks the resolution). Fails open (302 to the original).
//	@Tags			Proxy
//	@Param			u	query	string	true	"upstream image URL, base64url (no padding)"
//	@Produce		image/webp
//	@Router			/img [get]
func ImageProxy(w http.ResponseWriter, r *http.Request) {
	upstream := imgB64Decode(r.URL.Query().Get("u"))
	if upstream == "" || !strings.HasPrefix(upstream, "http") {
		http.Error(w, "bad u", http.StatusBadRequest)
		return
	}

	// SVG can't be raster-resized — hand it straight back.
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
			// The upstream fetch itself succeeded — we're holding valid
			// bytes, just couldn't re-encode them (animated WebP/GIF, AVIF,
			// any format image.Decode doesn't handle). A 302 to a foreign
			// host is a real problem for a *browser* client specifically:
			// unlike a native client, it enforces CORS on the request that
			// redirect sends it off to make on its own, and most upstream
			// image hosts don't set Access-Control-Allow-Origin — the
			// request gets blocked and the poster stays permanently black
			// (reported 2026-09-14: every plugin except one whose images
			// happen to already be same-origin). Serving the same bytes
			// ourselves, same-origin, costs nothing extra — we already paid
			// for the fetch — and sidesteps that failure mode entirely.
			//
			// Only bytes that sniff as a raster image are served inline, and
			// always under the SNIFFED type, never the upstream's own
			// Content-Type: /img is unauthenticated and shares the dashboard's
			// origin, so echoing an upstream "text/html" body verbatim ran
			// attacker script same-origin with /admin (one-click takeover)
			// and let anyone read arbitrary LAN responses through us.
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
		// Nothing to serve ourselves — the upstream fetch itself failed
		// (network error, timeout, non-2xx). A redirect is the only option
		// left; it just won't help a browser client any more than retrying
		// the same failed source directly would.
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

// imgShortURL trims the query string (tokens, cache-busters) and caps length
// so log lines stay readable.
func imgShortURL(u string) string {
	if p, err := url.Parse(u); err == nil && p.Path != "" {
		u = p.Host + p.Path
	}
	if len(u) > 96 {
		u = u[:93] + "..."
	}
	return u
}

// imgSecurityHeaders keeps whatever /img serves inert on this origin (it
// shares it with /admin): no MIME sniffing into HTML, and a sandbox CSP so
// even a mislabelled body can't run script or reach the dashboard's cookies.
func imgSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
}

// imgSniffRasterType returns the content type of raw when it is a raster
// image format a browser renders as an image (never SVG, which can carry
// script), or "" otherwise.
func imgSniffRasterType(raw []byte) string {
	// AVIF isn't in http.DetectContentType's table: ISO-BMFF "ftyp" box with
	// an avif/avis brand.
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

// imgFetchEncode downloads upstream, normalises to NRGBA (so the encoder
// always gets a format it handles and any alpha channel is kept), applies the
// safety cap, and encodes WebP. Also returns the source byte length and
// decoded format name for logging.
//
// rawBody/rawContentType are only set when the upstream fetch itself
// succeeded but re-encoding failed after that (an unknown/animated format —
// avif, animated webp/gif — image.Decode doesn't handle): the caller can
// serve those bytes back verbatim, same-origin, instead of redirecting to a
// foreign host a browser client's own CORS policy may then refuse to fetch.
// Left nil when the fetch itself failed — there's nothing to hand back.
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

	// Check the declared dimensions before decoding (decompression bomb, see
	// imgMaxSourcePixels). Oversized → plain error with no raw bytes: the
	// caller redirects to the original instead of serving it.
	if cfg, _, cerr := image.DecodeConfig(bytes.NewReader(raw)); cerr == nil {
		if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > imgMaxSourcePixels {
			return nil, srcLen, "", nil, "", fmt.Errorf("source too large: %dx%d", cfg.Width, cfg.Height)
		}
	}

	src, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, srcLen, "", raw, rawContentType, err // unknown/animated format → caller passes raw through
	}
	srcFmt = format

	dst := imgNormalize(src)

	var b bytes.Buffer
	// Method 4 = the library's balanced speed/size default (0 fastest … 6
	// smallest); encoding runs synchronously on the cache-miss request path.
	if err := webp.Encode(&b, dst, webp.Options{Quality: imgWebPQuality, Method: 4}); err != nil {
		return nil, srcLen, srcFmt, raw, rawContentType, err
	}
	return b.Bytes(), srcLen, srcFmt, nil, "", nil
}

// imgNormalize converts src to *image.NRGBA at its own resolution, or scales
// it down (aspect-preserving, Catmull-Rom, alpha kept) when either side
// exceeds imgMaxDimension. Never upscales.
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

// imgCacheSweep deletes oldest-mtime files once the cache dir exceeds
// imgCacheMaxBytes. Throttled to once/minute.
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
