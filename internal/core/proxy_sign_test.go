package core

import (
	"net/url"
	"path"
	"strconv"
	"testing"
	"time"
)

func TestProxySignRoundTrip(t *testing.T) {
	SetProxySignKey([]byte("test-master-secret"))
	defer SetProxySignKey(nil)

	raw := "https://box.local:8080/proxy/playlist.m3u8?data=aHR0cHM&origin=aHR0cA&cookies=Yz1k&vpn=1&egr=exit1&uid=sess123"
	signed := AppendProxySig(raw)

	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed: %v", err)
	}
	if u.Query().Get("sig") == "" {
		t.Fatal("AppendProxySig did not add a sig param")
	}
	if !VerifyProxyURL(path.Base(u.Path), u.Query()) {
		t.Fatal("freshly signed URL failed verification")
	}
}

func TestProxySignRejectsTamper(t *testing.T) {
	SetProxySignKey([]byte("test-master-secret"))
	defer SetProxySignKey(nil)

	signed := AppendProxySig("https://h/proxy/segment.ts?data=AAA&origin=BBB&cookies=CCC&vpn=0")
	u, _ := url.Parse(signed)

	// Swap the target: signature must no longer verify.
	q := u.Query()
	q.Set("data", "ZZZ")
	if VerifyProxyURL(path.Base(u.Path), q) {
		t.Fatal("tampered data still verified")
	}

	// uid is signed: swapping it breaks the signature.
	q = u.Query()
	q.Set("uid", "someone-else")
	if VerifyProxyURL(path.Base(u.Path), q) {
		t.Fatal("changing uid did not break verification — cross-profile tampering possible")
	}
}

// A URL signed for one uid fails verification once uid is swapped, with the
// same sig.
func TestProxySignUIDCoveredBySignature(t *testing.T) {
	SetProxySignKey([]byte("test-master-secret"))
	defer SetProxySignKey(nil)

	signed := AppendProxySig("https://h/proxy/segment.ts?data=AAA&origin=BBB&cookies=CCC&vpn=0&uid=profile-A")
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed: %v", err)
	}
	if !VerifyProxyURL(path.Base(u.Path), u.Query()) {
		t.Fatal("URL signed for profile-A failed to verify as-is")
	}

	// Attacker keeps `sig` from the URL they legitimately hold, and only
	// swaps uid to another profile's id.
	tampered := u.Query()
	tampered.Set("uid", "profile-B")
	if VerifyProxyURL(path.Base(u.Path), tampered) {
		t.Fatal("swapping uid=profile-A -> uid=profile-B kept the same sig valid")
	}
}

func TestProxySignFailsClosedWithoutKey(t *testing.T) {
	SetProxySignKey([]byte("k"))
	signed := AppendProxySig("https://h/proxy/key.key?data=AAA&vpn=0")
	u, _ := url.Parse(signed)

	SetProxySignKey(nil) // key gone
	if ProxySignEnabled() {
		t.Fatal("ProxySignEnabled true after clearing key")
	}
	if VerifyProxyURL(path.Base(u.Path), u.Query()) {
		t.Fatal("verified with no key configured")
	}
}

func TestProxySignMissingSig(t *testing.T) {
	SetProxySignKey([]byte("k"))
	defer SetProxySignKey(nil)
	q, _ := url.ParseQuery("data=AAA&origin=BBB&vpn=0")
	if VerifyProxyURL("segment.ts", q) {
		t.Fatal("verified a query with no sig param")
	}
}

// A URL signed for one endpoint doesn't verify on another (a subtitle or
// segment URL replayed on the playlist rewriter or the raw key fetch).
func TestProxySignBindsEndpoint(t *testing.T) {
	SetProxySignKey([]byte("k"))
	defer SetProxySignKey(nil)
	u, _ := url.Parse(AppendProxySig("https://h/proxy/subtitle.vtt?data=AAA"))
	if !VerifyProxyURL("subtitle.vtt", u.Query()) {
		t.Fatal("signed URL rejected on its own endpoint")
	}
	for _, other := range []string{"playlist.m3u8", "key.key", "segment.ts"} {
		if VerifyProxyURL(other, u.Query()) {
			t.Fatalf("subtitle URL verified on %s", other)
		}
	}
}

func TestProxySignExpires(t *testing.T) {
	SetProxySignKey([]byte("k"))
	defer SetProxySignKey(nil)
	u, _ := url.Parse(AppendProxySig("https://h/proxy/segment.ts?data=AAA"))
	q := u.Query()
	exp, _ := strconv.ParseInt(q.Get("exp"), 10, 64)
	if d := time.Until(time.Unix(exp, 0)); d <= 0 || d > ProxyURLTTL {
		t.Fatalf("exp %v from now, want within ProxyURLTTL", d)
	}
	q.Set("exp", strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
	if VerifyProxyURL("segment.ts", q) {
		t.Fatal("expired (or exp-tampered) URL verified")
	}
}

// Re-signing a query copied from a signed URL replaces its exp/sig.
func TestProxySignResignsCopiedQuery(t *testing.T) {
	SetProxySignKey([]byte("k"))
	defer SetProxySignKey(nil)
	first, _ := url.Parse(AppendProxySig("https://h/proxy/playlist.m3u8?data=AAA"))
	again, _ := url.Parse(AppendProxySig("https://h/proxy/playlist.m3u8?" + first.RawQuery))
	if n := len(again.Query()["exp"]); n != 1 {
		t.Fatalf("%d exp params after re-signing", n)
	}
	if !VerifyProxyURL("playlist.m3u8", again.Query()) {
		t.Fatal("re-signed URL rejected")
	}
}
