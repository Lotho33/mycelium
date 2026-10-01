package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mycelium/internal/core"
)

// TestFFmpegThroughProxy checks mycelium's own /proxy handlers serve
// everything an HLS client needs for an AES-128 encrypted stream with
// separate audio renditions and WebVTT subtitles (signed URLs, playlist
// rewriting, key proxy, segment relay): a real ffmpeg reads the whole thing
// through them. The offline downloader fetches through the same handlers.
// Skipped without MYCELIUM_TEST_FFMPEG_GEN.
func TestFFmpegThroughProxy(t *testing.T) {
	// A full ffmpeg build (network + encoders): the one shipped in the image
	// only muxes local files, see scripts/build-ffmpeg-min.sh.
	ff := os.Getenv("MYCELIUM_TEST_FFMPEG_GEN")
	if ff == "" {
		t.Skip("MYCELIUM_TEST_FFMPEG_GEN not set")
	}
	core.SetProxySignKey([]byte("e2e"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)
	useLoopbackSegmentClient(t)

	dir := t.TempDir()
	up := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer up.Close()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(ff, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("enc.key", "0123456789abcdef")
	write("keyinfo", up.URL+"/enc.key\n"+filepath.Join(dir, "enc.key")+"\n")
	hls := []string{"-f", "hls", "-hls_time", "2", "-hls_playlist_type", "vod"}
	run(append([]string{"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25", "-t", "6", "-an", "-c:v", "libx264", "-g", "25",
		"-hls_key_info_file", "keyinfo", "-hls_segment_filename", "v_%d.ts"}, append(hls, "video.m3u8")...)...)
	run(append([]string{"-f", "lavfi", "-i", "sine=frequency=440", "-t", "6", "-c:a", "aac",
		"-hls_segment_filename", "ita_%d.ts"}, append(hls, "ita.m3u8")...)...)
	write("subs.vtt", "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nCiao\n")
	write("subs.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:6.0,\nsubs.vtt\n#EXT-X-ENDLIST\n")
	write("master.m3u8", `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Italiano",LANGUAGE="ita",DEFAULT=YES,URI="ita.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="sub",NAME="Italiano",LANGUAGE="ita",URI="subs.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=600000,RESOLUTION=320x240,AUDIO="aud",SUBTITLES="sub"
video.m3u8
`)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /proxy/playlist.m3u8", ProxyPlaylist)
	mux.HandleFunc("GET /proxy/segment.ts", ProxySegment)
	mux.HandleFunc("GET /proxy/key.key", ProxyKey)
	proxy := httptest.NewServer(mux)
	defer proxy.Close()

	master := core.AppendProxySig(proxy.URL + "/proxy/playlist.m3u8?data=" + b64url(up.URL+"/master.m3u8") +
		"&origin=&cookies=&vpn=0&sid=" + url.QueryEscape(b64url("plug\x00s1")))
	// Children as the downloader picks them: read the proxied master.
	resp, err := http.Get(master)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(raw)
	var video, audio, subs string
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		switch {
		case strings.Contains(l, "TYPE=AUDIO"):
			audio = reKeyURI.FindStringSubmatch(l)[1]
		case strings.Contains(l, "TYPE=SUBTITLES"):
			subs = reKeyURI.FindStringSubmatch(l)[1]
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF") && i+1 < len(lines):
			video = strings.TrimSpace(lines[i+1])
		}
	}
	if video == "" || audio == "" || subs == "" {
		t.Fatalf("proxied master incomplete:\n%s", body)
	}

	out := filepath.Join(t.TempDir(), "out.mkv")
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}
	for _, in := range []string{video, audio, subs} {
		args = append(args, "-rw_timeout", "60000000", "-seg_max_retry", "5", "-extension_picky", "0", "-i", in)
	}
	args = append(args, "-map", "0:v:0", "-map", "1:a:0", "-map", "2:s:0?", "-c", "copy", "-c:s", "srt",
		"-metadata:s:a:0", "language=ita", "-f", "matroska", out)
	if b, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("download through proxy failed: %v\n%s", err, b)
	}
	info, _ := exec.Command(ff, "-hide_banner", "-i", out).CombinedOutput()
	for _, want := range []string{"Video: h264", "(ita): Audio: aac", "Subtitle: subrip"} {
		if !strings.Contains(string(info), want) {
			t.Errorf("output lacks %q:\n%s", want, info)
		}
	}
}
