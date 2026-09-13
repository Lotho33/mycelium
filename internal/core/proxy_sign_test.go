package core

import (
	"net/url"
	"testing"
)

func TestProxySignRoundTrip(t *testing.T) {
	SetProxySignKey([]byte("test-master-secret"))
	defer SetProxySignKey(nil)

	raw := "https://box.local:8080/proxy/playlist.m3u8?data=aHR0cHM&origin=aHR0cA&cookies=Yz1k&vpn=1&egr=mullvad&uid=sess123"
	signed := AppendProxySig(raw)

	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed: %v", err)
	}
	if u.Query().Get("sig") == "" {
		t.Fatal("AppendProxySig did not add a sig param")
	}
	if !VerifyProxyURL(u.Query()) {
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
	if VerifyProxyURL(q) {
		t.Fatal("tampered data still verified")
	}

	// uid IS signed — swapping it for another profile's id (the cross-profile
	// continue-watching tampering bug) must break the signature.
	q = u.Query()
	q.Set("uid", "someone-else")
	if VerifyProxyURL(q) {
		t.Fatal("changing uid did not break verification — cross-profile tampering possible")
	}
}

// TestProxySignUIDCoveredBySignature is the explicit regression test for the
// cross-profile "continue watching" tampering bug: a URL signed for profile A
// must fail verification once `uid` is swapped to profile B, even though
// `sig` itself is left untouched (exactly what an attacker holding a
// legitimately-signed /proxy/* URL and another profile's id — trivially
// obtained via ListProfiles — could do).
func TestProxySignUIDCoveredBySignature(t *testing.T) {
	SetProxySignKey([]byte("test-master-secret"))
	defer SetProxySignKey(nil)

	signed := AppendProxySig("https://h/proxy/segment.ts?data=AAA&origin=BBB&cookies=CCC&vpn=0&uid=profile-A")
	u, err := url.Parse(signed)
	if err != nil {
		t.Fatalf("parse signed: %v", err)
	}
	if !VerifyProxyURL(u.Query()) {
		t.Fatal("URL signed for profile-A failed to verify as-is")
	}

	// Attacker keeps `sig` from the URL they legitimately hold, and only
	// swaps uid to another profile's id.
	tampered := u.Query()
	tampered.Set("uid", "profile-B")
	if VerifyProxyURL(tampered) {
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
	if VerifyProxyURL(u.Query()) {
		t.Fatal("verified with no key configured")
	}
}

func TestProxySignMissingSig(t *testing.T) {
	SetProxySignKey([]byte("k"))
	defer SetProxySignKey(nil)
	q, _ := url.ParseQuery("data=AAA&origin=BBB&vpn=0")
	if VerifyProxyURL(q) {
		t.Fatal("verified a query with no sig param")
	}
}
