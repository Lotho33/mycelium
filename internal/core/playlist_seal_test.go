package core

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

var sealRule = PlaylistSeal{Cipher: SealCipherAES128CTR, Counter: SealCounterMediaSeq}

// seal encrypts uri the way the source does (see PlaylistSeal).
func seal(t *testing.T, key []byte, seq uint64, uri string) string {
	t.Helper()
	b, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv[8:], seq)
	out := make([]byte, len(uri))
	cipher.NewCTR(b, iv).XORKeyStream(out, []byte(uri))
	return base64.RawURLEncoding.EncodeToString(out)
}

func TestUnsealPlaylist(t *testing.T) {
	key := []byte("0123456789abcdef")
	segs := []string{
		"https://cdn.example/a~tplv.image?x=1&sig=abc",
		"https://cdn.example/b~tplv.image?x=2&sig=def",
		"https://cdn.example/c~tplv.image?x=3&sig=ghi",
	}
	pl := "#EXTM3U\n#EXT-X-TARGETDURATION:5\n#EXT-X-MEDIA-SEQUENCE:4294967298\n"
	for i, s := range segs {
		pl += "#EXTINF:5.000,\n" + seal(t, key, 4294967298+uint64(i), s) + "\n"
	}
	out, ok, err := UnsealPlaylist([]byte(pl), key, sealRule)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	for _, s := range segs {
		if !strings.Contains(string(out), "\n"+s+"\n") {
			t.Errorf("missing %s in\n%s", s, out)
		}
	}
	if !strings.Contains(string(out), "#EXT-X-MEDIA-SEQUENCE:4294967298") {
		t.Error("tags must stay")
	}

	// Wrong key: lines don't decrypt to URLs, the playlist is left alone.
	if got, ok, _ := UnsealPlaylist([]byte(pl), []byte("fedcba9876543210"), sealRule); ok || string(got) != pl {
		t.Error("a wrong key must leave the playlist unchanged")
	}
	// Wrong key size is an error, content unchanged.
	if got, ok, err := UnsealPlaylist([]byte(pl), []byte("short"), sealRule); ok || err == nil || string(got) != pl {
		t.Errorf("short key: ok=%v err=%v", ok, err)
	}
	// A playlist in clear passes through.
	clear := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:5,\nhttps://cdn.example/s1.ts\n"
	if got, ok, _ := UnsealPlaylist([]byte(clear), key, sealRule); ok || string(got) != clear {
		t.Error("clear playlist changed")
	}
	// A master is never touched.
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n" + seal(t, key, 0, "https://x/v.m3u8") + "\n"
	if _, ok, _ := UnsealPlaylist([]byte(master), key, sealRule); ok {
		t.Error("master changed")
	}
	// No key: nothing to do.
	if _, ok, err := UnsealPlaylist([]byte(pl), nil, sealRule); ok || err != nil {
		t.Error("nil key")
	}
}

func TestPlaylistSealKey(t *testing.T) {
	k := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
	u := "https://edge.example/premium31/index.m3u8?t=1.abc#mpk=" + k
	if got := string(PlaylistSealKey(u)); got != "0123456789abcdef" {
		t.Errorf("key %q", got)
	}
	if f := PlaylistSealFragment(u); f != "#mpk="+k {
		t.Errorf("fragment %q", f)
	}
	for _, u := range []string{"https://x/a.m3u8", "https://x/a.m3u8#other=1", "https://x/a.m3u8#mpk=%%%"} {
		if PlaylistSealKey(u) != nil {
			t.Errorf("%s: key found", u)
		}
	}
	if PlaylistSealFragment("https://x/a.m3u8#other=1") != "" {
		t.Error("unrelated fragment passed on")
	}
}

func TestPlaylistSealValidate(t *testing.T) {
	if sealRule.Validate() != nil {
		t.Error("valid rule refused")
	}
	for _, r := range []PlaylistSeal{{Cipher: "rc4", Counter: SealCounterMediaSeq}, {Cipher: SealCipherAES128CTR, Counter: "zero"}} {
		if r.Validate() == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}
