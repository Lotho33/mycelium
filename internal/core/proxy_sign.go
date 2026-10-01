package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/url"
	"strings"
)

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
	m.Write([]byte("mycelium/proxy-url-sig/v1"))
	proxySignKey = m.Sum(nil)
}

// ProxySignEnabled reports whether a signing key has been installed.
func ProxySignEnabled() bool { return len(proxySignKey) > 0 }

// proxySigInput is the canonical signed string of a /proxy/* URL: the
// security-relevant query params in a fixed order, one per line (no
// concatenation ambiguity), read url-decoded as the handler reads them.
// uid, the playback session token, is included so a signed URL can't be
// moved onto another session.
func proxySigInput(q url.Values) string {
	return strings.Join([]string{
		q.Get("data"),
		q.Get("origin"),
		q.Get("cookies"),
		q.Get("xhdr"),
		q.Get("vpn"),
		q.Get("egr"),
		q.Get("sid"),
		q.Get("uid"),
	}, "\n")
}

// SignProxyURL returns the hex HMAC-SHA256 of q's canonical form.
func SignProxyURL(q url.Values) string {
	mac := hmac.New(sha256.New, proxySignKey)
	mac.Write([]byte(proxySigInput(q)))
	return hex.EncodeToString(mac.Sum(nil))
}

// AppendProxySig parses rawURL, signs its query, and returns rawURL with
// &sig=<hex> appended. On a parse failure it returns rawURL unchanged — the
// verify side then rejects it, which is the safe outcome.
func AppendProxySig(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	sep := "?"
	if u.RawQuery != "" {
		sep = "&"
	}
	return rawURL + sep + "sig=" + SignProxyURL(u.Query())
}

// VerifyProxyURL reports whether q carries a valid `sig` for its params. Returns
// false when no signing key is configured (fail closed).
func VerifyProxyURL(q url.Values) bool {
	if len(proxySignKey) == 0 {
		return false
	}
	want := SignProxyURL(q)
	got := q.Get("sig")
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
