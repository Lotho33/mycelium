package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// ProxyURLTTL is how long a signed /proxy URL stays valid. A playlist
// re-fetched past it fails and the client resolves the stream again; the
// URLs a playlist hands out are signed afresh each time it is served.
const ProxyURLTTL = 48 * time.Hour

// proxySignKey is the key of the HMAC on /proxy/* URLs, derived from the
// master secret at boot (SetProxySignKey). Empty until then: VerifyProxyURL
// fails closed.
var proxySignKey []byte

// SetProxySignKey installs the signing key, derived from master so it is never
// the same bytes as master's other uses (JWT signing). Call once at startup.
func SetProxySignKey(master []byte) {
	if len(master) == 0 {
		proxySignKey = nil
		return
	}
	m := hmac.New(sha256.New, master)
	m.Write([]byte("mycelium/proxy-url-sig/v2"))
	proxySignKey = m.Sum(nil)
}

// ProxySignEnabled reports whether a signing key has been installed.
func ProxySignEnabled() bool { return len(proxySignKey) > 0 }

// proxySigInput is the canonical signed string of a /proxy/* URL: the
// endpoint (last path element) and the security-relevant query params in a
// fixed order, one per line (no concatenation ambiguity), read url-decoded
// as the handler reads them. The endpoint is covered so a URL signed for one
// handler (a subtitle file, a segment) can't be replayed on another (the
// playlist rewriter, the raw key fetch); uid, the playback session token, so
// it can't be moved onto another session; exp, so it doesn't live forever.
func proxySigInput(endpoint string, q url.Values) string {
	s := strings.Join([]string{
		endpoint,
		q.Get("exp"),
		q.Get("data"),
		q.Get("origin"),
		q.Get("cookies"),
		q.Get("xhdr"),
		q.Get("vpn"),
		q.Get("egr"),
		q.Get("sid"),
		q.Get("uid"),
	}, "\n")
	// xsub (external subtitle tracks, fetched server-side) only when present.
	if x := q.Get("xsub"); x != "" {
		s += "\nxsub=" + x
	}
	return s
}

func signProxy(endpoint string, q url.Values) string {
	mac := hmac.New(sha256.New, proxySignKey)
	mac.Write([]byte(proxySigInput(endpoint, q)))
	return hex.EncodeToString(mac.Sum(nil))
}

// AppendProxySig parses rawURL, adds an expiry (ProxyURLTTL from now), signs
// it for its endpoint and returns rawURL with &exp=…&sig=<hex> appended. On a
// parse failure it returns rawURL unchanged — the verify side then rejects
// it, which is the safe outcome.
func AppendProxySig(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	if q.Has("sig") || q.Has("exp") {
		// Re-signing a query copied from a signed request: drop the old pair.
		q.Del("sig")
		q.Del("exp")
		u.RawQuery = q.Encode()
		rawURL = u.String()
	}
	exp := strconv.FormatInt(time.Now().Add(ProxyURLTTL).Unix(), 10)
	q.Set("exp", exp)
	sep := "?"
	if u.RawQuery != "" {
		sep = "&"
	}
	return rawURL + sep + "exp=" + exp + "&sig=" + signProxy(path.Base(u.Path), q)
}

// VerifyProxyURL reports whether q carries a valid, unexpired `sig` for its
// params on endpoint (the request path's last element). Returns false when
// no signing key is configured (fail closed).
func VerifyProxyURL(endpoint string, q url.Values) bool {
	if len(proxySignKey) == 0 {
		return false
	}
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return false
	}
	want := signProxy(endpoint, q)
	got := q.Get("sig")
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
