package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/url"
	"strings"
)

// proxySignKey is the derived key used to HMAC the /proxy/* URLs mycelium mints
// (playlist / segment / key). It is set once at boot from the Pileus master
// secret via SetProxySignKey. Empty until then — VerifyProxyURL fails closed.
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

// proxySigInput is the canonical string signed for a /proxy/* URL: the
// security-relevant query params in a fixed order, newline-separated. `uid`
// IS included (fixed position, last) — it is the real Pileus profileID and is
// used server-side as the key into managers.Sessions.Register (one active
// stream per profile) and as the userID in managers.DB.UpsertProgress
// ("continue watching"). Leaving it out of the signature let anyone holding a
// legitimately-signed /proxy/* URL swap `uid` for another profile's id
// (trivially discoverable via ListProfiles) without invalidating `sig`, and
// so poison another profile's watch history / session on the same account.
// Values are read url-decoded, exactly as the serving handler reads them via
// r.URL.Query(). Newline-joining a fixed list of fields (rather than raw
// query-string concatenation) means there's no a=1&ab=2 vs a=12&b= ambiguity:
// each field occupies its own line regardless of its content.
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
