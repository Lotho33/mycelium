package downloads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRunBlobEndToEnd runs the real downloader + ffmpeg mux against a
// generated HLS stream shaped like the real sources — AES-128 encrypted
// video-only variant, two separate audio renditions, WebVTT subtitles — and
// interrupts it halfway to check the job resumes without fetching again what
// it already has. MYCELIUM_FFMPEG is the ffmpeg used for muxing (the one
// shipped in the image); MYCELIUM_TEST_FFMPEG_GEN, if set, a full build used
// only to generate the test stream (the shipped one has no encoders).
func TestRunBlobEndToEnd(t *testing.T) {
	ff := os.Getenv("MYCELIUM_FFMPEG")
	if ff == "" {
		t.Skip("MYCELIUM_FFMPEG not set")
	}
	gen := os.Getenv("MYCELIUM_TEST_FFMPEG_GEN")
	if gen == "" {
		gen = ff
	}
	e := newEnv(t)
	dir := t.TempDir()

	// Upstream: counts segment requests, can cut the job after N of them.
	var mu sync.Mutex
	hits := map[string]int{}
	var stopAfter int
	var stop context.CancelFunc
	files := http.FileServer(http.Dir(dir))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".ts") || strings.HasSuffix(r.URL.Path, ".vtt") {
			mu.Lock()
			hits[r.URL.Path]++
			total := 0
			for _, n := range hits {
				total += n
			}
			if stopAfter > 0 && total >= stopAfter && stop != nil {
				stop()
			}
			mu.Unlock()
		}
		files.ServeHTTP(w, r)
	}))
	defer up.Close()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gen, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
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
	run(append([]string{"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25", "-t", "12", "-an", "-c:v", "libx264", "-g", "25",
		"-hls_key_info_file", "keyinfo", "-hls_segment_filename", "v_%d.ts"}, append(hls, "video.m3u8")...)...)
	run(append([]string{"-f", "lavfi", "-i", "sine=frequency=440", "-t", "12", "-c:a", "aac",
		"-hls_segment_filename", "ita_%d.ts"}, append(hls, "ita.m3u8")...)...)
	run(append([]string{"-f", "lavfi", "-i", "sine=frequency=880", "-t", "12", "-c:a", "aac",
		"-hls_segment_filename", "jpn_%d.ts"}, append(hls, "jpn.m3u8")...)...)
	write("subs.vtt", "WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nCiao mondo\n")
	write("subs.m3u8", "#EXTM3U\n#EXT-X-TARGETDURATION:12\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:12.0,\nsubs.vtt\n#EXT-X-ENDLIST\n")
	write("master.m3u8", `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Italiano",LANGUAGE="ita",DEFAULT=YES,URI="ita.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Japanese",LANGUAGE="jpn",URI="jpn.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="sub",NAME="Italiano",LANGUAGE="ita",URI="subs.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=600000,RESOLUTION=320x240,AUDIO="aud",SUBTITLES="sub"
video.m3u8
`)

	m := e.m
	m.ffmpeg = ff
	m.runJob = m.runBlob
	m.resolve = func(ctx context.Context, p, s, o string) (Source, error) {
		return Source{URL: up.URL + "/master.m3u8", IsHLS: true}, nil
	}
	in := e.create(t, "p", func(r *CreateRequest) {
		r.VariantID = variantID(Variant{Height: 240, Bandwidth: 600000})
		r.AudioIDs = []string{"a:jpn:Japanese", "a:ita:Italiano"}
		r.SubtitleIDs = []string{"s:ita:Italiano"}
		r.Title = "Prova"
	})
	it, _ := loadItem("", in.ID)
	b, _ := loadBlob(it.BlobID)

	// First run, cut after 5 segment requests (as a restart would).
	ctx, cancel := context.WithCancel(context.Background())
	mu.Lock()
	stopAfter, stop = 5, cancel
	mu.Unlock()
	if err := m.runBlob(ctx, b, false); err == nil {
		t.Fatal("interrupted run reported success")
	}
	mu.Lock()
	firstHits := map[string]int{}
	for k, v := range hits {
		firstHits[k] = v
	}
	stopAfter, stop = 0, nil
	mu.Unlock()

	// Second run resumes.
	if err := m.runBlob(context.Background(), b, false); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	mu.Lock()
	refetched := 0
	for k, n := range hits {
		if firstHits[k] > 0 && n > firstHits[k] {
			refetched++
		}
	}
	mu.Unlock()
	if refetched > 1 {
		t.Errorf("%d segments fetched again after resume (at most the one in flight)", refetched)
	}

	got, _ := m.Get("p", in.ID)
	if got.Status != StatusCompleted || got.FileBytes == 0 {
		t.Fatalf("after resume %+v", got)
	}
	if _, err := os.Stat(m.workDir(b.ID)); !os.IsNotExist(err) {
		t.Error("work dir left behind")
	}
	info, _ := exec.Command(ff, "-hide_banner", "-i", m.finalPath(b.ID)).CombinedOutput()
	s := string(info)
	for _, want := range []string{"Video: h264", "(jpn): Audio: aac", "(ita): Audio: aac", "(ita): Subtitle: subrip", "matroska"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Index(s, "(jpn): Audio") > strings.Index(s, "(ita): Audio") {
		t.Error("audio order doesn't follow the selection (jpn first)")
	}
}
