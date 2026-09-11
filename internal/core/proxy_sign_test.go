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

	// uid is not signed — changing it must NOT break the signature.
	q = u.Query()
	q.Set("uid", "someone-else")
	if !VerifyProxyURL(q) {
		t.Fatal("changing the unsigned uid param broke verification")
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
