package pileus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

func TestCalendarRange(t *testing.T) {
	prev := calendarNow
	calendarNow = func() time.Time { return time.Date(2026, 11, 24, 15, 0, 0, 0, time.Local) }
	t.Cleanup(func() { calendarNow = prev })

	f, to, err := calendarRange("", "")
	if err != nil || f.Format("2006-01-02") != "2026-11-24" || to.Format("2006-01-02") != "2026-11-30" {
		t.Fatalf("default = %v %v %v", f, to, err)
	}
	f, to, _ = calendarRange("2026-11-01", "2027-03-01")
	if to.Format("2006-01-02") != "2026-12-01" {
		t.Fatalf("capped to = %v", to)
	}
	if _, _, err := calendarRange("2026-11-10", "2026-11-01"); err == nil {
		t.Fatal("to before from accepted")
	}
	if _, _, err := calendarRange("24/11/2026", ""); err == nil {
		t.Fatal("bad date accepted")
	}
}

func TestParseReleaseAt(t *testing.T) {
	cases := []struct {
		in     string
		allDay bool
		ok     bool
	}{
		{"2026-11-25", true, true},
		{"2026-11-25T15:30", false, true},
		{"2026-11-25 15:30:00", false, true},
		{"2026-11-25T15:30:00+09:00", false, true},
		{"2026-11-25T15:30Z", false, true},
		{"domani", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		_, allDay, ok := parseReleaseAt(c.in)
		if ok != c.ok || allDay != c.allDay {
			t.Errorf("%q: allDay=%v ok=%v", c.in, allDay, ok)
		}
	}
	jp, _, _ := parseReleaseAt("2026-11-25T15:30:00+09:00")
	if !jp.Equal(time.Date(2026, 11, 25, 6, 30, 0, 0, time.UTC)) {
		t.Errorf("offset lost: %v", jp)
	}
}

func loadCalendarPlugin(t *testing.T, id, src string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "id: " + id + "\nname: T\ndirect_egress: true\npool_size: 1\nentrypoints:\n  calendar: calendar\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "init.lua"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := engine.LuaPlugins.LoadPlugin(dir); err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	t.Cleanup(func() { engine.LuaPlugins.UnloadPlugin(id) })
}

func TestGetCalendar(t *testing.T) {
	setupNewEpisodesDB(t)
	calendarMu.Lock()
	calendarCache = map[string]calendarCacheEntry{}
	calendarMu.Unlock()
	if err := managers.DB.AddToMyList("a", managers.MyListEntry{PluginID: "t.cal", MediaID: "show1", MediaType: "series"}); err != nil {
		t.Fatal(err)
	}
	loadCalendarPlugin(t, "t.cal", `
		calls = 0
		function calendar(args)
			calls = calls + 1
			local f = table.concat(args.follows or {}, ",")
			return {
				{ id = "show1_1_2", title = "Ep 2", media_type = "episode", show_id = "show1",
				  season_number = 1, episode_number = 2, release_at = "2026-11-26T15:30", available = false },
				{ id = "film", title = "Film " .. f, media_type = "movie", release_at = "2026-11-26" },
				{ id = "show2_3_1", title = "Altra", media_type = "episode", series_id = "show2",
				  release_at = "2026-11-25 20:00", available = true },
				{ id = "fuori", title = "Fuori", release_at = "2026-12-25" },
				{ id = "senza", title = "Senza data" },
				{ id = "calls", title = tostring(calls), release_at = "2026-11-27" },
			}
		end
	`)
	ctx := context.WithValue(context.Background(), ctxKeyDeviceID{}, "a")
	h := &MediaHandler{}
	resp, err := h.GetCalendar(ctx, &gen.CalendarRequest{From: "2026-11-24", To: "2026-11-30"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.FailedPlugins) != 0 || len(resp.Entries) != 4 {
		t.Fatalf("entries = %v, failed = %v", resp.Entries, resp.FailedPlugins)
	}
	ids := []string{}
	for _, e := range resp.Entries {
		ids = append(ids, e.Item.Id)
	}
	// by time; on the same day the undated (all day) entry first
	want := []string{"show2_3_1", "film", "show1_1_2", "calls"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order = %v, want %v", ids, want)
		}
	}
	ep := resp.Entries[2]
	if !ep.Followed || ep.SeriesId != "show1" || ep.Available || ep.AllDay || ep.Item.SeasonNumber != 1 {
		t.Errorf("followed episode = %+v", ep)
	}
	if resp.Entries[0].Followed || !resp.Entries[0].Available {
		t.Errorf("other series = %+v", resp.Entries[0])
	}
	if resp.Entries[1].Item.Title != "Film show1" || !resp.Entries[1].AllDay {
		t.Errorf("follows not passed / all day lost: %+v", resp.Entries[1])
	}

	// Cached: the plugin isn't asked again.
	again, _ := h.GetCalendar(ctx, &gen.CalendarRequest{From: "2026-11-24", To: "2026-11-30"})
	if again.Entries[3].Item.Title != "1" {
		t.Errorf("plugin asked again: calls = %s", again.Entries[3].Item.Title)
	}
	// Only the followed series.
	only, _ := h.GetCalendar(ctx, &gen.CalendarRequest{From: "2026-11-24", To: "2026-11-30", FollowedOnly: true})
	if len(only.Entries) != 1 || only.Entries[0].Item.Id != "show1_1_2" {
		t.Errorf("followed only = %v", only.Entries)
	}
	// The cached entry was not marked for this profile's request.
	b := context.WithValue(context.Background(), ctxKeyDeviceID{}, "b")
	other, _ := h.GetCalendar(b, &gen.CalendarRequest{From: "2026-11-24", To: "2026-11-30"})
	for _, e := range other.Entries {
		if e.Followed {
			t.Errorf("profile b sees followed: %+v", e)
		}
	}
}

func TestGetCalendarFailingPlugin(t *testing.T) {
	setupNewEpisodesDB(t)
	loadCalendarPlugin(t, "t.calfail", `function calendar(args) error("sito giù") end`)
	ctx := context.WithValue(context.Background(), ctxKeyDeviceID{}, "a")
	resp, err := (&MediaHandler{}).GetCalendar(ctx, &gen.CalendarRequest{PluginId: "t.calfail"})
	if err != nil || len(resp.FailedPlugins) != 1 || len(resp.Entries) != 0 {
		t.Fatalf("resp = %v, %v", resp, err)
	}
}

func TestCalendarAnswerTTL(t *testing.T) {
	ep := &gen.CalendarEntry{Item: &gen.CatalogItem{MediaType: "series"}}
	live := &gen.CalendarEntry{Item: &gen.CatalogItem{MediaType: "live"}}
	if got := calendarAnswerTTL([]*gen.CalendarEntry{ep}, nil); got != calendarTTL {
		t.Errorf("series = %v", got)
	}
	if got := calendarAnswerTTL([]*gen.CalendarEntry{ep, live}, nil); got != calendarLiveTTL {
		t.Errorf("with a live item = %v", got)
	}
	if got := calendarAnswerTTL(nil, errors.New("x")); got != calendarFailTTL {
		t.Errorf("failure = %v", got)
	}
}
