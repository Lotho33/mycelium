package downloads

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// File URLs are signed and short-lived: whoever holds one can fetch the file
// with no other credential (players and download managers can't add a
// Bearer header), so it must not be guessable nor valid forever. ListDownloads
// mints a fresh one on every call.
const fileURLTTL = 24 * time.Hour

var (
	signMu  sync.RWMutex
	signKey []byte
)

// SetSignKey derives this package's HMAC key from the server master secret
// (domain-separated, like the /proxy and admin-session keys).
func SetSignKey(master []byte) {
	signMu.Lock()
	defer signMu.Unlock()
	if len(master) == 0 {
		signKey = nil
		return
	}
	m := hmac.New(sha256.New, master)
	m.Write([]byte("mycelium/downloads/v1"))
	signKey = m.Sum(nil)
}

func sign(id string, exp int64, ver string) string {
	signMu.RLock()
	defer signMu.RUnlock()
	m := hmac.New(sha256.New, signKey)
	m.Write([]byte(id + "\n" + strconv.FormatInt(exp, 10) + "\n" + ver))
	return hex.EncodeToString(m.Sum(nil))
}

// signedFilePath is "/downloads/file?id=…&v=…&exp=…&sig=…". v is the file
// version the URL was minted for (fileVersion): an upgraded file gets new
// links, so a resumed download never mixes two files.
func signedFilePath(id, ver string, exp time.Time) string {
	e := exp.Unix()
	q := url.Values{"id": {id}, "v": {ver}, "exp": {strconv.FormatInt(e, 10)}, "sig": {sign(id, e, ver)}}
	return "/downloads/file?" + q.Encode()
}

func verifyFileQuery(q url.Values, now time.Time) (id, ver string, ok bool) {
	signMu.RLock()
	keyed := len(signKey) > 0
	signMu.RUnlock()
	id, ver, expRaw, sig := q.Get("id"), q.Get("v"), q.Get("exp"), q.Get("sig")
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if !keyed || id == "" || err != nil || now.Unix() > exp {
		return "", "", false
	}
	return id, ver, subtle.ConstantTimeCompare([]byte(sign(id, exp, ver)), []byte(sig)) == 1
}

// fileVersion identifies one version of a finished file (its mtime: the
// rename that installs a file, first or upgraded, sets a new one).
func fileVersion(fi os.FileInfo) string {
	return strconv.FormatInt(fi.ModTime().UnixNano(), 36)
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N} ._()-]+`)

func downloadFileName(it *item) string {
	name := it.Title
	if it.SeriesTitle != "" {
		name = it.SeriesTitle
		if it.Season > 0 && it.Episode > 0 {
			name += " S" + pad2(it.Season) + "E" + pad2(it.Episode)
		}
		if it.Title != "" && it.Title != it.SeriesTitle {
			name += " - " + it.Title
		}
	}
	name = strings.TrimSpace(unsafeName.ReplaceAllString(name, ""))
	if name == "" {
		name = "download"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name + ".mkv"
}

func pad2(n int32) string {
	if n < 10 {
		return "0" + strconv.Itoa(int(n))
	}
	return strconv.Itoa(int(n))
}

// ServeFile is GET /downloads/file: the finished file, with Range support
// (http.ServeContent), so device download managers can resume and a TV can
// seek while playing straight from the server.
func (m *Manager) ServeFile(w http.ResponseWriter, r *http.Request) {
	id, ver, ok := verifyFileQuery(r.URL.Query(), m.now())
	if !ok {
		http.Error(w, "link non valido o scaduto", http.StatusForbidden)
		return
	}
	it, err := loadItem("", id)
	if err != nil {
		http.Error(w, "download non disponibile", http.StatusNotFound)
		return
	}
	b, err := loadBlob(it.BlobID)
	if err != nil || b.Status != StatusCompleted {
		http.Error(w, "download non disponibile", http.StatusNotFound)
		return
	}
	f, err := os.Open(m.finalPath(b.ID))
	if err != nil {
		http.Error(w, "file non trovato", http.StatusNotFound)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "file non leggibile", http.StatusInternalServerError)
		return
	}
	if fileVersion(fi) != ver {
		// Replaced by a better-quality version since this link was minted:
		// a resumed download would mix the two files. The client lists its
		// downloads again and starts over on the new link.
		http.Error(w, "il file è stato sostituito da una versione migliore: richiedi di nuovo il link", http.StatusGone)
		return
	}
	name := downloadFileName(it)
	w.Header().Set("Content-Type", "video/x-matroska")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}
