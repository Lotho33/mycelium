package api

import (
	"crypto/aes"
	"crypto/cipher"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/h264/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The SPS gives the real picture size, cropping included (1080 is coded as
// 1088 lines).
func TestVideoDims(t *testing.T) {
	for _, c := range []struct {
		file string
		w, h int
	}{{"1440.ts", 1920, 1440}, {"1080.ts", 1920, 1080}, {"init720.mp4", 1280, 720}} {
		w, h, ok := videoDims(readTestdata(t, c.file))
		if !ok || w != c.w || h != c.h {
			t.Errorf("%s: %dx%d ok=%v, want %dx%d", c.file, w, h, ok, c.w, c.h)
		}
	}
	if _, _, ok := videoDims([]byte("not media at all")); ok {
		t.Error("garbage must not yield dimensions")
	}
}

func aesCBCEncrypt(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	pad := 16 - len(plain)%16
	padded := append(append([]byte{}, plain...), make([]byte, pad)...)
	for i := len(plain); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(out, padded)
	return out
}

// An AES-128 segment is decrypted with the playlist's key; without an IV
// attribute the IV is the segment's media sequence number.
func TestProbeVariantDims_Encrypted(t *testing.T) {
	useLoopbackPlaylistClient(t)
	key := []byte("0123456789abcdef")
	keySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(key) }))
	defer keySrv.Close()

	iv := make([]byte, 16)
	iv[15] = 7 // media sequence 7
	enc := aesCBCEncrypt(t, readTestdata(t, "1440.ts"), key, iv)
	media := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:7\n#EXT-X-KEY:METHOD=AES-128,URI=\"" + keySrv.URL + "/k\"\n#EXTINF:4,\nseg7.ts\n#EXTINF:4,\nseg8.ts\n"
	w, h, ok := probeVariantDims(t.Context(), getProxySession(), managers.PrefetchOptions{}, "https://cdn.example/v/720.m3u8", []byte(media), "https://cdn.example/v/seg7.ts", enc)
	if !ok || w != 1920 || h != 1440 {
		t.Fatalf("got %dx%d ok=%v, want 1920x1440", w, h, ok)
	}
}

func TestCorrectStreamInfResolution(t *testing.T) {
	line := `#EXT-X-STREAM-INF:BANDWIDTH=4500000,RESOLUTION=1920x1080,CODECS="avc1.640028"`
	got, declared, changed := correctStreamInfResolution(line, 1920, 1440)
	if !changed || declared != "1920x1080" || !strings.Contains(got, "RESOLUTION=1920x1440,") {
		t.Fatalf("got %q declared %q changed %v", got, declared, changed)
	}
	if _, _, changed := correctStreamInfResolution(got, 1920, 1440); changed {
		t.Fatal("a correct RESOLUTION must stay as is")
	}
	got, _, changed = correctStreamInfResolution("#EXT-X-STREAM-INF:BANDWIDTH=1", 1280, 720)
	if !changed || got != "#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1280x720" {
		t.Fatalf("missing RESOLUTION: got %q", got)
	}
}

// ProxyPlaylist rewrites a master with the measured RESOLUTION of a variant.
func TestProxyPlaylist_CorrectsMeasuredResolution(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=4500000,RESOLUTION=1920x1080\nhi/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=1800000,RESOLUTION=1280x720\nmid/index.m3u8\n"))
	}))
	defer upstream.Close()
	rememberVariantDims(upstream.URL+"/m/hi/index.m3u8", 1920, 1440)
	rememberVariantDims(upstream.URL+"/m/mid/index.m3u8", 1280, 534) // letterboxed: smaller, left as declared

	rec := httptest.NewRecorder()
	ProxyPlaylist(rec, httptest.NewRequest(http.MethodGet, core.AppendProxySig("http://box.local/proxy/playlist.m3u8?data="+b64url(upstream.URL+"/m/master.m3u8")+"&origin=&cookies=&vpn=0"), nil))
	body := rec.Body.String()
	if !strings.Contains(body, "BANDWIDTH=4500000,RESOLUTION=1920x1440") || !strings.Contains(body, "RESOLUTION=1280x720") {
		t.Fatalf("master not corrected:\n%s", body)
	}
}

// The pre-buffer measures the top variant from its first segment.
func TestPrefetchHLSHead_MeasuresTopVariant(t *testing.T) {
	managers.Segments.Reset()
	t.Cleanup(managers.Segments.Reset)
	seg := readTestdata(t, "1440.ts")
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=4500000,RESOLUTION=1920x1080\nhi/media.m3u8\n"))
	})
	mux.HandleFunc("/hi/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\nseg0.ts\n#EXTINF:4,\nseg1.ts\n#EXT-X-ENDLIST\n"))
	})
	mux.HandleFunc("/hi/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/MP2T")
		w.Write(seg)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := &fakeUpstream{srv: srv}
	withFakeClients(t, f)

	res := prefetchHLSHead(t.Context(), srv.URL+"/master.m3u8", managers.PrefetchOptions{MaxSegments: 2}, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	w, h, ok := lookupVariantDims(srv.URL + "/hi/media.m3u8")
	if !ok || w != 1920 || h != 1440 {
		t.Fatalf("measured %dx%d ok=%v, want 1920x1440", w, h, ok)
	}
}
