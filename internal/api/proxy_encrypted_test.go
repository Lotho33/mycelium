package api

import (
	"crypto/aes"
	"crypto/cipher"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mycelium/internal/core"
)

// encryptedSegURL is signedSegmentURL plus the enc=1 flag ProxyPlaylist adds
// for segments that follow an #EXT-X-KEY, re-signed over the full query.
func encryptedSegURL(upstreamURL string) string {
	return core.AppendProxySig("http://box.local/proxy/segment.ts?data=" + b64url(upstreamURL) + "&origin=&cookies=&vpn=0&enc=1")
}

// aesCBCTS returns an AES-128-CBC encrypted MPEG-TS-like payload (PKCS#7),
// which is what vixsrc serves under .html/.webp/.jpg names.
func aesCBCTS(t *testing.T) (cipherText, plain []byte) {
	t.Helper()
	plain = tsPayload(300)
	pad := 16 - len(plain)%16
	padded := append(append([]byte{}, plain...), make([]byte, pad)...)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	blk, err := aes.NewCipher([]byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	cipherText = make([]byte, len(padded))
	cipher.NewCBCEncrypter(blk, make([]byte, 16)).CryptBlocks(cipherText, padded)
	return cipherText, plain
}

func TestProxySegment_EncryptedRelayedVerbatimDespiteHTMLContentType(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)

	ct, _ := aesCBCTS(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(ct)
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	ProxySegment(rec, httptest.NewRequest(http.MethodGet, encryptedSegURL(upstream.URL+"/video/720p/0000-0100.html"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "video/") {
		t.Fatalf("Content-Type = %q, want video/*", rec.Header().Get("Content-Type"))
	}
	if got := rec.Body.Bytes(); string(got) != string(ct) {
		t.Fatalf("body altered: got %d bytes, want %d verbatim", len(got), len(ct))
	}
}

func TestProxySegment_EncryptedButTextPageIsRejected(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!DOCTYPE html><html><body>Just a moment...</body></html>"))
	}))
	defer upstream.Close()

	rec := httptest.NewRecorder()
	ProxySegment(rec, httptest.NewRequest(http.MethodGet, encryptedSegURL(upstream.URL+"/x.html"), nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a challenge page", rec.Code)
	}
}

func TestKeyLineEncrypts(t *testing.T) {
	for line, want := range map[string]bool{
		`#EXT-X-KEY:METHOD=AES-128,URI="/storage/enc.key",IV=0x43A6D967D5C17290D98322F5C8F6660B`: true,
		`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="k"`:                                                   true,
		`#EXT-X-KEY:METHOD=NONE`:                                                                 false,
		`#EXTINF:4,`:                                                                             false,
	} {
		if got := keyLineEncrypts(line); got != want {
			t.Errorf("keyLineEncrypts(%q) = %v, want %v", line, got, want)
		}
	}
	if !playlistEncrypted([]byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:4,\nseg.html\n")) {
		t.Error("playlistEncrypted: want true")
	}
	if playlistEncrypted([]byte("#EXTM3U\n#EXTINF:4,\nseg.ts\n")) {
		t.Error("playlistEncrypted: want false")
	}
}

func TestProxyPlaylist_TagsSegmentsOfEncryptedPlaylist(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:5\n" +
			"#EXT-X-KEY:METHOD=AES-128,URI=\"/storage/enc.key\",IV=0x43A6D967D5C17290D98322F5C8F6660B\n" +
			"#EXTINF:4,\nseg0.html\n#EXTINF:4,\nseg1.html\n#EXT-X-ENDLIST\n"))
	}))
	defer upstream.Close()

	u := core.AppendProxySig("http://box.local/proxy/playlist.m3u8?data=" + b64url(upstream.URL+"/pl/media.m3u8") + "&origin=&cookies=&vpn=0")
	rec := httptest.NewRecorder()
	ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, u, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	var segs, tagged int
	for _, l := range strings.Split(rec.Body.String(), "\n") {
		switch {
		case strings.Contains(l, "/proxy/segment.ts"):
			segs++
			if strings.Contains(l, "&enc=1") {
				tagged++
			}
		case strings.HasPrefix(l, "#EXT-X-KEY"):
			if strings.Contains(l, "enc=1") || !strings.Contains(l, "/proxy/key.key") {
				t.Errorf("key line must go through key.key untagged: %s", l)
			}
		}
	}
	if segs != 2 || tagged != 2 {
		t.Fatalf("segments=%d tagged=%d, want 2/2\n%s", segs, tagged, rec.Body.String())
	}
}
