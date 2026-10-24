package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mycelium/internal/core"
)

func TestInjectExtSubs_AddsGroupAndSkipsKnownLanguage(t *testing.T) {
	master := []string{
		"#EXTM3U",
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="English",LANGUAGE="eng",URI="x"`,
		"#EXT-X-STREAM-INF:BANDWIDTH=1000,AUDIO=\"a\"",
		"http://p/v1",
		"#EXT-X-STREAM-INF:BANDWIDTH=2000,AUDIO=\"a\"",
		"http://p/v2",
	}
	subs := []core.ExtSubtitle{{URL: "https://s/it.srt", Lang: "ita", Name: "Italiano", Default: true}}
	out := strings.Join(injectExtSubs(master, subs, "http://h/proxy"), "\n")
	if !strings.Contains(out, `TYPE=SUBTITLES,GROUP-ID="mycelium-xsub",NAME="Italiano",DEFAULT=YES,AUTOSELECT=YES,LANGUAGE="it",URI="http://h/proxy/subtitle.m3u8?data=`) {
		t.Fatalf("rendition missing:\n%s", out)
	}
	if strings.Count(out, `SUBTITLES="mycelium-xsub"`) != 2 {
		t.Fatalf("variants not tied to the group:\n%s", out)
	}

	// The master already has (non-forced) Italian subtitles: nothing added.
	withIta := append([]string{}, master[:2]...)
	withIta = append(withIta, `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="s",NAME="Ita",LANGUAGE="it",URI="y"`,
		`#EXT-X-STREAM-INF:BANDWIDTH=1000,SUBTITLES="s"`, "http://p/v1")
	if got := injectExtSubs(withIta, subs, "http://h/proxy"); len(got) != len(withIta) {
		t.Fatalf("duplicate language injected: %v", got)
	}

	// Only forced Italian: the full track is added to the existing group.
	forced := append([]string{}, master[:2]...)
	forced = append(forced, `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="s",NAME="Ita forced",LANGUAGE="it",FORCED=YES,URI="y"`,
		`#EXT-X-STREAM-INF:BANDWIDTH=1000,SUBTITLES="s"`, "http://p/v1")
	out = strings.Join(injectExtSubs(forced, subs, "http://h/proxy"), "\n")
	if !strings.Contains(out, `GROUP-ID="s",NAME="Italiano"`) || strings.Contains(out, "mycelium-xsub") {
		t.Fatalf("forced-only master not completed in its own group:\n%s", out)
	}
}

func TestToWebVTT_SRT(t *testing.T) {
	srt := "1\r\n00:00:01,500 --> 00:00:03,000\r\n{\\an8}<font color=\"#fff\">Ciao</font> \xe8 qui\r\n\r\n2\r\n01:02:03,000 --> 01:02:04,250\r\n<i>Fine</i>\r\n"
	vtt, err := toWebVTT([]byte(srt))
	if err != nil {
		t.Fatal(err)
	}
	s := string(vtt)
	if !strings.HasPrefix(s, "WEBVTT\n\n") || !strings.Contains(s, "00:00:01.500 --> 00:00:03.000") ||
		!strings.Contains(s, "Ciao è qui") || !strings.Contains(s, "<i>Fine</i>") {
		t.Fatalf("bad conversion:\n%s", s)
	}
	if d := vttDuration(vtt); d != 3724.25 {
		t.Fatalf("duration = %v, want 3724.25", d)
	}
}

func TestProxySubtitle_EndToEnd(t *testing.T) {
	core.SetProxySignKey([]byte("k"))
	defer core.SetProxySignKey(nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1\n00:00:00,000 --> 00:00:09,000\nCiao\n"))
	}))
	defer up.Close()
	// Loopback is SSRF-blocked on the real client: plain client for the test.
	upstreamMu.Lock()
	old := proxySession
	proxySession = &http.Client{}
	upstreamMu.Unlock()
	t.Cleanup(func() {
		upstreamMu.Lock()
		proxySession = old
		upstreamMu.Unlock()
	})

	pl := httptest.NewRecorder()
	ProxySubtitlePlaylist(pl, httptest.NewRequest("GET", core.AppendProxySig("/proxy/subtitle.m3u8?data="+b64url(up.URL+"/a.srt")), nil))
	body := pl.Body.String()
	if !strings.Contains(body, "#EXT-X-ENDLIST") || !strings.Contains(body, "/proxy/subtitle.vtt?data=") {
		t.Fatalf("playlist:\n%s", body)
	}
	if !strings.Contains(body, "#EXTINF:10.000,") {
		t.Fatalf("duration not taken from the last cue:\n%s", body)
	}
	vttURL := body[strings.Index(body, "/proxy/subtitle.vtt"):]
	vttURL = vttURL[:strings.Index(vttURL, "\n")]
	f := httptest.NewRecorder()
	ProxySubtitleFile(f, httptest.NewRequest("GET", vttURL, nil))
	if got := f.Body.String(); f.Header().Get("Content-Type") != "text/vtt; charset=utf-8" || !strings.Contains(got, "00:00:00.000 --> 00:00:09.000\nCiao") {
		t.Fatalf("vtt (%s):\n%s", f.Header().Get("Content-Type"), got)
	}

	// A failing upstream gives an empty but valid track, never an error.
	bad := httptest.NewRecorder()
	ProxySubtitlePlaylist(bad, httptest.NewRequest("GET", core.AppendProxySig("/proxy/subtitle.m3u8?data="+b64url("http://127.0.0.1:1/x.srt")), nil))
	if bad.Code != http.StatusOK || !strings.Contains(bad.Body.String(), "#EXT-X-ENDLIST") {
		t.Fatalf("failing subtitle → %d:\n%s", bad.Code, bad.Body.String())
	}
}

func TestExtSubs_SignatureCoversXsub(t *testing.T) {
	core.SetProxySignKey([]byte("k"))
	defer core.SetProxySignKey(nil)
	x := core.EncodeExtSubs([]core.ExtSubtitle{{URL: "https://s/it.srt", Lang: "it"}})
	signed := core.AppendProxySig("http://h/proxy/playlist.m3u8?data=YQ&xsub=" + x)
	r := httptest.NewRequest("GET", signed, nil)
	if !core.VerifyProxyURL("playlist.m3u8", r.URL.Query()) {
		t.Fatal("signed xsub URL rejected")
	}
	tampered := strings.Replace(signed, "xsub="+x, "xsub="+core.EncodeExtSubs([]core.ExtSubtitle{{URL: "http://169.254.169.254/"}}), 1)
	if core.VerifyProxyURL("playlist.m3u8", httptest.NewRequest("GET", tampered, nil).URL.Query()) {
		t.Fatal("tampered xsub accepted")
	}
	// A URL signed without xsub verifies too.
	plain := core.AppendProxySig("http://h/proxy/playlist.m3u8?data=YQ")
	if !core.VerifyProxyURL("playlist.m3u8", httptest.NewRequest("GET", plain, nil).URL.Query()) {
		t.Fatal("plain URL rejected")
	}
}

func TestProxyPlaylist_ExtSubs(t *testing.T) {
	core.SetProxySignKey([]byte("k"))
	defer core.SetProxySignKey(nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nv.m3u8\n"))
		default:
			w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\ns0.ts\n#EXT-X-ENDLIST\n"))
		}
	}))
	defer up.Close()
	upstreamMu.Lock()
	old := proxySession
	proxySession = &http.Client{}
	upstreamMu.Unlock()
	t.Cleanup(func() {
		upstreamMu.Lock()
		proxySession = old
		upstreamMu.Unlock()
	})
	x := core.EncodeExtSubs([]core.ExtSubtitle{{URL: "https://s/it.srt", Lang: "it", Name: "Italiano", Default: true}})
	get := func(path string) string {
		rec := httptest.NewRecorder()
		u := core.AppendProxySig("http://box.local/proxy/playlist.m3u8?data=" + b64url(up.URL+path) + "&origin=&cookies=&vpn=0&xsub=" + x)
		ProxyPlaylist(rec, httptest.NewRequest("GET", u, nil))
		return rec.Body.String()
	}

	if m := get("/master.m3u8"); !strings.Contains(m, "TYPE=SUBTITLES") || !strings.Contains(m, `BANDWIDTH=1000,SUBTITLES="mycelium-xsub"`) {
		t.Fatalf("master without the subtitle rendition:\n%s", m)
	}
	// A bare media playlist is wrapped in a one-variant master whose child
	// URL no longer carries xsub (no wrapping loop).
	m := get("/media.m3u8")
	if !strings.Contains(m, "TYPE=SUBTITLES") || !strings.Contains(m, "#EXT-X-STREAM-INF:BANDWIDTH=2000000") {
		t.Fatalf("media playlist not wrapped:\n%s", m)
	}
	for _, l := range strings.Split(m, "\n") {
		if strings.HasPrefix(l, "http://box.local/proxy/playlist.m3u8") && strings.Contains(l, "xsub=") {
			t.Fatalf("child still carries xsub: %s", l)
		}
	}
}

// Plugin-supplied subtitle names and languages can't break out of their tag.
func TestInjectExtSubs_SanitizesNameAndLanguage(t *testing.T) {
	out := injectExtSubs([]string{"#EXTM3U", "#EXT-X-STREAM-INF:BANDWIDTH=1", "v.m3u8"},
		[]core.ExtSubtitle{{URL: "https://s/x.srt", Lang: "it\n#EXT-X-KEY", Name: "Ita\r\n#EXT-X-ENDLIST\""}}, "http://h/proxy")
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "\n#EXT-X-ENDLIST") || strings.Contains(joined, "\n#EXT-X-KEY") {
		t.Fatalf("injected tag:\n%s", joined)
	}
}

// An inline subtitle (a file the plugin handed itself) is served from memory,
// cp1252 bytes included, with no upstream request.
func TestProxySubtitle_Inline(t *testing.T) {
	core.SetProxySignKey([]byte("k"))
	defer core.SetProxySignKey(nil)
	u, ok := core.PutInlineSubtitle([]byte("1\n00:00:01,000 --> 00:00:04,000\nPerch\xe9\n"))
	if !ok || core.EncodeExtSubs([]core.ExtSubtitle{{URL: u}}) == "" {
		t.Fatalf("inline url %q not accepted", u)
	}
	f := httptest.NewRecorder()
	ProxySubtitleFile(f, httptest.NewRequest("GET", core.AppendProxySig("/proxy/subtitle.vtt?data="+b64url(u)), nil))
	if got := f.Body.String(); f.Code != http.StatusOK || !strings.Contains(got, "00:00:01.000 --> 00:00:04.000\nPerché") {
		t.Fatalf("vtt (%d):\n%s", f.Code, got)
	}
}
