package pileus

import (
	"context"
	"database/sql"
	"testing"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"

	"mycelium/internal/managers"
)

const newEpWatchHistory = `CREATE TABLE watch_history (
	client_id TEXT, provider_id TEXT, playable_id TEXT, title TEXT, poster TEXT,
	progress_time REAL, total_time REAL, is_completed BOOLEAN,
	last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
	parent_id TEXT NOT NULL DEFAULT '', navigation_context TEXT NOT NULL DEFAULT '',
	rating REAL NOT NULL DEFAULT 0, genres TEXT NOT NULL DEFAULT '', plot TEXT NOT NULL DEFAULT '',
	year INTEGER NOT NULL DEFAULT 0, logo_url TEXT NOT NULL DEFAULT '',
	season_number INTEGER NOT NULL DEFAULT 0, episode_number INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (client_id, provider_id, playable_id));`

func setupNewEpisodesDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, s := range []string{newEpWatchHistory, managers.MyListSchema, managers.SeriesLatestSchema} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	prev := managers.DB
	managers.DB = managers.NewDBManager(db)
	t.Cleanup(func() { db.Close(); managers.DB = prev })
	return db
}

func TestNewEpisodesFor(t *testing.T) {
	db := setupNewEpisodesDB(t)
	now := time.Now()
	day := int64(24 * 3600)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	watched := func(owner, series string, s, e int, daysAgo int64) {
		exec(`INSERT INTO watch_history(client_id, provider_id, playable_id, parent_id, navigation_context,
			title, poster, progress_time, total_time, is_completed, season_number, episode_number, last_updated)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?, datetime(?, 'unixepoch'))`,
			owner, "p", series+"_ep", series, "Serie "+series, "ep", "poster", 90, 100, 1, s, e, now.Unix()-daysAgo*day)
	}
	latest := func(series string, s, e int, foundDaysAgo int64) {
		found := now.Unix() - foundDaysAgo*day
		if foundDaysAgo < 0 {
			found = 0
		}
		exec(`INSERT INTO series_latest VALUES(?,?,?,?,?,?,?)`, "p", series, s, e, "2026-11-20", found, now.Unix())
	}

	watched("a", "caught_up", 1, 7, 5) // watched E7 5 days ago…
	latest("caught_up", 1, 8, 2)       // …E8 found 2 days ago: new
	watched("a", "behind", 1, 3, 1)    // watching E3 yesterday…
	latest("behind", 1, 8, 2)          // …E8 found before that: not news
	watched("a", "current", 1, 8, 5)   // already on E8
	latest("current", 1, 8, 2)         // not past where it got to
	watched("a", "old", 1, 1, 30)
	latest("old", 1, 2, 20) // found 20 days ago: out of the window
	watched("a", "first", 1, 1, 5)
	latest("first", 1, 9, -1) // first check (found_at 0): never news
	if err := managers.DB.AddToMyList("a", managers.MyListEntry{PluginID: "p", MediaID: "listed", MediaType: "series", Title: "In lista"}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE my_list SET added_at = ? WHERE media_id = 'listed'`, now.Unix()-10*day)
	latest("listed", 3, 1, 1) // never watched, added 10 days ago, S3E1 found yesterday: new

	items, err := newEpisodesFor("a", now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*gen.NewEpisode{}
	for _, it := range items {
		got[it.SeriesId] = it
	}
	if len(got) != 2 || got["caught_up"] == nil || got["listed"] == nil {
		t.Fatalf("new episodes = %v; want caught_up and listed", items)
	}
	if c := got["caught_up"]; c.EpisodeNumber != 8 || c.Title != "Serie caught_up" || c.PosterUrl != "poster" {
		t.Errorf("caught_up = %+v", c)
	}
	if items[0].SeriesId != "listed" {
		t.Errorf("most recent first: %v", items)
	}
	// Another profile follows nothing.
	if other, _ := newEpisodesFor("b", now); len(other) != 0 {
		t.Errorf("profile b = %v", other)
	}

	// Badges on the lists.
	ctx := context.WithValue(context.Background(), ctxKeyDeviceID{}, "a")
	h := &MediaHandler{}
	ml, err := h.GetMyList(ctx, &gen.MyListRequest{})
	if err != nil || len(ml.Items) != 1 || ml.Items[0].Badge != BadgeNewEpisode {
		t.Fatalf("my list = %v, %v", ml, err)
	}
	// Continue Watching lists unfinished rows only: mark two of them so.
	exec(`UPDATE watch_history SET is_completed = 0, progress_time = 40 WHERE parent_id IN ('caught_up', 'behind')`)
	cw, err := h.GetContinueWatching(ctx, &gen.ContinueWatchingRequest{Limit: 50})
	if err != nil || len(cw.Items) != 2 {
		t.Fatalf("continue watching = %v, %v", cw, err)
	}
	for _, it := range cw.Items {
		want := ""
		if it.ParentId == "caught_up" {
			want = BadgeNewEpisode
		}
		if it.Badge != want {
			t.Errorf("continue watching %s: badge %q, want %q", it.ParentId, it.Badge, want)
		}
	}
	ne, err := h.GetNewEpisodes(ctx, &gen.NewEpisodesRequest{})
	if err != nil || len(ne.Items) != 2 {
		t.Fatalf("GetNewEpisodes = %v, %v", ne, err)
	}
}
