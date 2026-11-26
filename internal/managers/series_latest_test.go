package managers

import "testing"

func TestSetSeriesLatest(t *testing.T) {
	m := NewDBManager(newTestDB(t, MyListSchema+SeriesLatestSchema))
	get := func() SeriesLatest {
		all, err := m.AllSeriesLatest()
		if err != nil {
			t.Fatal(err)
		}
		return all[[2]string{"p", "s"}]
	}
	// First check: what is out then is not news.
	if newer, err := m.SetSeriesLatest("p", "s", 1, 5, "2026-11-01"); err != nil || newer {
		t.Fatalf("first: newer=%v err=%v", newer, err)
	}
	if l := get(); l.Episode != 5 || l.FoundAt != 0 || l.CheckedAt == 0 {
		t.Fatalf("first: %+v", l)
	}
	// Same episode, then nothing reported: only checked_at moves.
	if newer, _ := m.SetSeriesLatest("p", "s", 1, 5, ""); newer {
		t.Fatal("same episode reported as newer")
	}
	if newer, _ := m.SetSeriesLatest("p", "s", 0, 0, ""); newer || get().Episode != 5 {
		t.Fatal("an empty answer must not erase the episode")
	}
	// A later one: found now.
	if newer, _ := m.SetSeriesLatest("p", "s", 2, 1, "2026-11-20"); !newer {
		t.Fatal("S2E1 after S1E5 not newer")
	}
	if l := get(); l.Season != 2 || l.Episode != 1 || l.FoundAt == 0 || l.AirDate != "2026-11-20" {
		t.Fatalf("later: %+v", l)
	}
	// An earlier one (the source lost an episode for a while): kept as is.
	if newer, _ := m.SetSeriesLatest("p", "s", 1, 9, ""); newer || get().Season != 2 {
		t.Fatal("an earlier episode replaced the stored one")
	}
}

func TestFollowedSeries(t *testing.T) {
	m := NewDBManager(newTestDB(t, MyListSchema+SeriesLatestSchema))
	if err := m.AddToMyList("a", MyListEntry{PluginID: "p", MediaID: "s1", MediaType: "series"}); err != nil {
		t.Fatal(err)
	}
	_ = m.AddToMyList("a", MyListEntry{PluginID: "p", MediaID: "m1", MediaType: "movie"})
	if err := m.UpsertProgress("b", "p", "s2_1_3", "s2", "Serie 2", "T", "", 10, 100, 0, nil, "", 0, 1, 3); err != nil {
		t.Fatal(err)
	}
	_ = m.UpsertProgress("b", "p", "mov", "", "", "Film", "", 10, 100, 0, nil, "", 0, 0, 0)
	due, err := m.FollowedSeries(90, 1<<62)
	if err != nil || len(due) != 2 {
		t.Fatalf("followed = %v, %v; want s1 and s2", due, err)
	}
	// Checked just now but without a card (a row from before cards): due
	// once more, to fetch it.
	_, _ = m.SetSeriesLatest("p", "s1", 1, 1, "")
	due, _ = m.FollowedSeries(90, 1)
	if len(due) != 2 {
		t.Fatalf("due = %v; want s1 (no card) and s2", due)
	}
	// Checked just now, card stored (even an empty one): not due.
	_ = m.SetSeriesCard("p", "s1", nil)
	due, _ = m.FollowedSeries(90, 1) // s2 never checked
	if len(due) != 1 || due[0][1] != "s2" {
		t.Fatalf("due = %v; want only s2", due)
	}
	ws, err := m.SeriesWatchState("b")
	if w := ws[[2]string{"p", "s2"}]; err != nil || w.Season != 1 || w.Episode != 3 || w.LastWatched == 0 || w.Title != "Serie 2" {
		t.Fatalf("watch state = %+v, %v", w, err)
	}
}
