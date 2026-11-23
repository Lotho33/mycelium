package api

import (
	"encoding/json"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrewarmable(t *testing.T) {
	cases := map[string]bool{
		"https://img.example/cover.jpg":                                        true,
		"mycelium:/img/versus?h=aaa&a=bbb":                                     true,
		"https://image.tmdb.org/t/p/w342/x.jpg":                                false,
		"http://jf:8096/Items/0123456789abcdef0123456789abcdef/Images/Primary": false,
		"https://img.example/logo.svg":                                         false,
		"data:image/png;base64,xx":                                             false,
		"":                                                                     false,
	}
	for u, want := range cases {
		if got := prewarmable(u); got != want {
			t.Errorf("prewarmable(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestPrewarmCatalogImages_FillsTheCache(t *testing.T) {
	withPlainImgClient(t)
	red := solidPNG(t, color.NRGBA{220, 30, 30, 255}, 64, 64)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(red) //nolint:errcheck
	}))
	defer srv.Close()

	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	cover := srv.URL + "/cover.png?" + nonce
	home, away := srv.URL+"/h.png?"+nonce, srv.URL+"/a.png?"+nonce
	versus := "mycelium:/img/versus?h=" + b64(home) + "&a=" + b64(away)
	items := []map[string]any{
		{"id": "1", "poster_url": cover, "extra": map[string]any{"fanart_url": versus, "is_live": "1"}},
		{"id": "2", "poster_url": "https://image.tmdb.org/t/p/w342/x.jpg"},
	}
	raw, _ := json.Marshal(map[string]any{"items": items, "has_more": false})
	prewarmCatalogImages("p", "c", raw)

	wait := func(f string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if fi, err := os.Stat(f); err == nil && fi.Size() > 0 {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("not prewarmed: %s", f)
	}
	wait(imgCacheFile(cover))
	wait(versusCacheFile(home, away))

	// A client now finds both ready: no new upstream fetch.
	before := hits.Load()
	rec := httptest.NewRecorder()
	ImageProxy(rec, imgReq(cover))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("cover: status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec = httptest.NewRecorder()
	ImageVersus(rec, versusReq(home, away))
	if rec.Code != http.StatusOK {
		t.Fatalf("versus: status %d", rec.Code)
	}
	if hits.Load() != before {
		t.Fatalf("client requests fetched upstream again (%d → %d)", before, hits.Load())
	}
}

// With every conversion slot taken by clients, the prewarm waits instead of
// queueing behind them, and converts once one is free.
func TestPrewarmOne_YieldsToClients(t *testing.T) {
	withPlainImgClient(t)
	red := solidPNG(t, color.NRGBA{30, 200, 30, 255}, 32, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(red) //nolint:errcheck
	}))
	defer srv.Close()
	u := srv.URL + "/busy.png?" + strconv.FormatInt(time.Now().UnixNano(), 36)

	for i := 0; i < cap(imgConvSem); i++ {
		imgConvSem <- struct{}{}
	}
	got := make(chan bool, 1)
	go func() { got <- prewarmOne(u) }()
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(imgCacheFile(u)); err == nil {
		t.Fatal("prewarm converted while every slot was taken")
	}
	for i := 0; i < cap(imgConvSem); i++ {
		<-imgConvSem
	}
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("prewarm gave up once a slot was free")
		}
	case <-time.After(imgPrewarmSlotWait + 5*time.Second):
		t.Fatal("prewarm never took the freed slot")
	}
}
