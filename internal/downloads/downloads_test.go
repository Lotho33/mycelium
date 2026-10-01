package downloads

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testMaster = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Italiano",LANGUAGE="ita",DEFAULT=YES,URI="audio/ita.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Japanese",LANGUAGE="jpn",URI="audio/jpn.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="sub",NAME="Italiano",LANGUAGE="ita",URI="subs/ita.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=854x480,CODECS="avc1.4d401f",AUDIO="aud",SUBTITLES="sub"
480/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,AVERAGE-BANDWIDTH=4000000,RESOLUTION=1920x1080,CODECS="avc1.640028",AUDIO="aud",SUBTITLES="sub"
1080/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,AUDIO="aud",SUBTITLES="sub"
720/index.m3u8
`

const testMedia = "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\ns1.ts\n#EXTINF:6.0,\ns2.ts\n#EXTINF:3.5,\ns3.ts\n#EXT-X-ENDLIST\n"

func TestParsePlaylist_MasterSortedWithRenditions(t *testing.T) {
	m := ParsePlaylist(testMaster, "http://h/hls/master.m3u8")
	if !m.IsMaster || len(m.Variants) != 3 {
		t.Fatalf("variants: %+v", m.Variants)
	}
	v := m.Variants[0]
	if v.Height != 1080 || v.Bandwidth != 4000000 || v.Peak || v.URI != "http://h/hls/1080/index.m3u8" {
		t.Fatalf("best variant %+v", v)
	}
	if m.Variants[2].Height != 480 || !m.Variants[2].Peak {
		t.Fatalf("worst variant %+v", m.Variants[2])
	}
	if len(m.groupRenditions("AUDIO", "aud")) != 2 || len(m.groupRenditions("SUBTITLES", "sub")) != 1 {
		t.Fatalf("renditions %+v", m.Renditions)
	}
	if m.Renditions[0].URI != "http://h/hls/audio/ita.m3u8" || !m.Renditions[0].Default {
		t.Fatalf("rendition %+v", m.Renditions[0])
	}
}

func TestParsePlaylist_MediaPlaylistIsItsOwnVariant(t *testing.T) {
	m := ParsePlaylist(testMedia, "http://h/v.m3u8")
	if m.IsMaster || len(m.Variants) != 1 || m.Variants[0].URI != "http://h/v.m3u8" {
		t.Fatalf("%+v", m)
	}
	d, ended := MediaDuration(testMedia)
	if d != 15.5 || !ended {
		t.Fatalf("duration %v ended %v", d, ended)
	}
	if _, ended := MediaDuration("#EXTM3U\n#EXTINF:6,\na.ts\n"); ended {
		t.Fatal("live playlist reported as ended")
	}
}

func TestPickVariant(t *testing.T) {
	vs := ParsePlaylist(testMaster, "http://h/m.m3u8").Variants
	cases := []struct {
		sel  Selection
		want int
	}{
		{Selection{}, 1080},
		{Selection{Height: 720, Bandwidth: 2500000}, 720},
		{Selection{Height: 900}, 720},   // gone: best below
		{Selection{Height: 2160}, 1080}, // above everything: best below
		{Selection{Height: 240}, 480},   // below everything: lowest
	}
	for _, c := range cases {
		if v, _ := pickVariant(vs, c.sel); v.Height != c.want {
			t.Errorf("sel %+v → %d, want %d", c.sel, v.Height, c.want)
		}
	}
}

func TestIDsRoundTrip(t *testing.T) {
	h, bw, ok := parseVariantID(variantID(Variant{Height: 1080, Bandwidth: 4000000}))
	if !ok || h != 1080 || bw != 4000000 {
		t.Fatal("variant id")
	}
	c, ok := parseTrackID(trackID(Rendition{Type: "AUDIO", Language: "ita", Name: "Italiano: 5.1"}), "a")
	if !ok || c.Language != "ita" || c.Name != "Italiano: 5.1" {
		t.Fatalf("track id %+v", c)
	}
	if _, ok := parseTrackID("s:ita:x", "a"); ok {
		t.Fatal("subtitle id accepted as audio")
	}
}

func TestWindow(t *testing.T) {
	w, ok := parseWindow("23:30-06:00")
	if !ok {
		t.Fatal("parse")
	}
	at := func(h, m int) time.Time { return time.Date(2026, 9, 29, h, m, 0, 0, time.UTC) }
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(23, 45), true}, {at(3, 0), true}, {at(6, 0), false}, {at(12, 0), false}} {
		if w.contains(c.t) != c.want {
			t.Errorf("%v contains=%v", c.t, !c.want)
		}
	}
	if got := w.nextStart(at(12, 0)); !got.Equal(at(23, 30)) {
		t.Errorf("nextStart noon = %v", got)
	}
	if got := w.nextStart(at(2, 0)); !got.Equal(at(2, 0)) {
		t.Errorf("nextStart inside window = %v", got)
	}
	w2, _ := parseWindow("02:00-07:00")
	if got := w2.nextStart(at(8, 0)); !got.Equal(at(2, 0).AddDate(0, 0, 1)) {
		t.Errorf("nextStart after window = %v", got)
	}
	for _, bad := range []string{"", "2-7", "25:00-07:00", "02:00-02:00"} {
		if _, ok := parseWindow(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPlanAndArgs(t *testing.T) {
	m := ParsePlaylist(testMaster, "http://h/m.m3u8")

	// Nothing chosen: the group's default audio is added (video-only variant).
	_, in := plan(m, Selection{Height: 720, Bandwidth: 2500000})
	if len(in) != 2 || in[1].Kind != "audio" || in[1].Language != "ita" {
		t.Fatalf("default audio not added: %+v", in)
	}

	sel := Selection{Height: 1080, Bandwidth: 4000000,
		Audio:     []TrackCriterion{{"jpn", "Japanese"}, {"ita", "Italiano"}},
		Subtitles: []TrackCriterion{{"ita", ""}}} // name changed since: language still matches
	v, in := plan(m, sel)
	if v.Height != 1080 || len(in) != 4 || in[1].Language != "jpn" || in[3].Kind != "subtitle" {
		t.Fatalf("plan %+v", in)
	}
	args := strings.Join(buildFFmpegArgs(in, muxInputOpts, "/tmp/o.mkv.part", "Serie 1x01"), " ")
	for _, want := range []string{
		"-map 0:v:0", "-map 1:a:0", "-map 2:a:0", "-map 3:s:0?", "-c copy", "-c:s srt",
		"-metadata:s:a:0 language=jpn", "-metadata:s:a:1 language=ita", "-metadata:s:s:0 language=ita",
		"-disposition:a:0 default", "-f matroska /tmp/o.mkv.part", "-extension_picky 0", "-allowed_extensions ALL",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "0:a?") {
		t.Error("muxed audio mapped although separate audio was chosen")
	}

	// A media playlist (audio inside): all its audio is kept.
	single := ParsePlaylist(testMedia, "http://h/v.m3u8")
	_, in = plan(single, Selection{})
	if a := strings.Join(buildFFmpegArgs(in, nil, "o", ""), " "); !strings.Contains(a, "-map 0:a?") {
		t.Errorf("single playlist args: %s", a)
	}
}

func TestProgressTracker(t *testing.T) {
	var got []float64
	var size int64
	var ended bool
	progressTracker{onUpdate: func(s float64, b int64, end bool) { got = append(got, s); size = b; ended = end }}.consume(strings.NewReader(
		"out_time_us=25000000\ntotal_size=1000\nprogress=continue\nout_time_us=50000000\ntotal_size=2000\nprogress=continue\nprogress=end\n"))
	if len(got) != 3 || got[0] != 25 || got[1] != 50 || size != 2000 || !ended {
		t.Fatalf("progress %v size %d end %v", got, size, ended)
	}
}

func TestSignedFilePath(t *testing.T) {
	SetSignKey([]byte("master"))
	defer SetSignKey(nil)
	now := time.Now()
	p := signedFilePath("abc", "v1", now.Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, p, nil)
	if id, ver, ok := verifyFileQuery(req.URL.Query(), now); !ok || id != "abc" || ver != "v1" {
		t.Fatal("valid signature refused")
	}
	if _, _, ok := verifyFileQuery(req.URL.Query(), now.Add(2*time.Hour)); ok {
		t.Fatal("expired link accepted")
	}
	q := req.URL.Query()
	q.Set("id", "other")
	if _, _, ok := verifyFileQuery(q, now); ok {
		t.Fatal("tampered id accepted")
	}
	q = req.URL.Query()
	q.Set("v", "v2")
	if _, _, ok := verifyFileQuery(q, now); ok {
		t.Fatal("tampered version accepted")
	}
}

func TestParseMediaAndLocalize(t *testing.T) {
	const pl = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:6
#EXT-X-MAP:URI="init.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="https://k/key?t=1",IV=0x01
#EXTINF:6.0,
a.m4s
#EXTINF:4.0,
b.m4s
#EXT-X-ENDLIST
`
	mp := ParseMedia(pl, "http://h/p/v.m3u8")
	if !mp.Ended || len(mp.Segments) != 2 || mp.Duration != 10 || mp.Segments[1].URI != "http://h/p/b.m4s" {
		t.Fatalf("%+v", mp)
	}
	local, res := mp.Localize("i0", "http://h/p/v.m3u8")
	for _, want := range []string{`#EXT-X-MAP:URI="i0-m000.seg"`, `URI="i0-k000.key",IV=0x01`, "i0-s00000.seg", "i0-s00001.seg", "#EXT-X-ENDLIST", "#EXT-X-TARGETDURATION:6"} {
		if !strings.Contains(local, want) {
			t.Errorf("local playlist lacks %q:\n%s", want, local)
		}
	}
	if strings.Contains(local, "https://") || strings.Contains(local, "http://") {
		t.Errorf("remote URL left in local playlist:\n%s", local)
	}
	kinds := map[string]int{}
	for _, r := range res {
		kinds[r.Kind]++
	}
	if kinds["map"] != 1 || kinds["key"] != 1 || kinds["segment"] != 2 || res[len(res)-1].End != 10 {
		t.Fatalf("resources %+v", res)
	}
	if ParseMedia("#EXTM3U\n#EXT-X-BYTERANGE:100@0\n#EXTINF:1,\na.ts\n", "http://h/x").ByteRange != true {
		t.Fatal("byte range not detected")
	}
}

// A key URI in single quotes (or bare) is still localized, and no other
// URI-bearing tag survives into the playlist ffmpeg reads with file access.
func TestLocalize_NoRemoteOrLocalPathURIsLeft(t *testing.T) {
	const pl = "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-SESSION-DATA:DATA-ID=\"x\",URI=\"/etc/passwd\"\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI='/app/data/config.json'\n#EXTINF:6,\na.ts\n" +
		"#EXT-X-MAP:URI=init.mp4\n#EXTINF:6,\nb.ts\n#EXT-X-ENDLIST\n"
	local, res := ParseMedia(pl, "http://h/p/index.m3u8").Localize("i0", "http://h/p/index.m3u8")
	if strings.Contains(local, "/etc/passwd") || strings.Contains(local, "config.json") || strings.Contains(local, "init.mp4") {
		t.Fatalf("remote/local path left in playlist:\n%s", local)
	}
	var key, mp bool
	for _, r := range res {
		key = key || (r.Kind == "key" && r.URL == "http://h/app/data/config.json")
		mp = mp || (r.Kind == "map" && r.URL == "http://h/p/init.mp4")
	}
	if !key || !mp {
		t.Fatalf("key/map not localized: %+v", res)
	}
}
