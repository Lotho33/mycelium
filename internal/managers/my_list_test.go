package managers

import "testing"

func TestMyList_AddRefreshRemove(t *testing.T) {
	m := NewDBManager(newTestDB(t, MyListSchema))

	if err := m.AddToMyList("u1", MyListEntry{PluginID: "p", MediaID: "a", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddToMyList("u1", MyListEntry{PluginID: "q", MediaID: "b", Title: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddToMyList("u2", MyListEntry{PluginID: "p", MediaID: "a", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	// Re-adding refreshes the card, keeps a single row.
	if err := m.AddToMyList("u1", MyListEntry{PluginID: "p", MediaID: "a", Title: "A2"}); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetMyList("u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("u1 has %d entries, want 2", len(got))
	}
	titles := map[string]string{}
	for _, e := range got {
		titles[e.MediaID] = e.Title
	}
	if titles["a"] != "A2" {
		t.Fatalf("title of a = %q, want refreshed A2", titles["a"])
	}
	if only, _ := m.GetMyList("u1", "q"); len(only) != 1 || only[0].MediaID != "b" {
		t.Fatalf("plugin filter = %+v", only)
	}

	if err := m.RemoveFromMyList("u1", "p", "a"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveFromMyList("u1", "p", "a"); err != nil { // idempotent
		t.Fatal(err)
	}
	if left, _ := m.GetMyList("u1", ""); len(left) != 1 {
		t.Fatalf("after remove: %d entries, want 1", len(left))
	}
	if err := m.DeleteMyListForOwner("u1"); err != nil {
		t.Fatal(err)
	}
	if left, _ := m.GetMyList("u1", ""); len(left) != 0 {
		t.Fatalf("after owner delete: %d entries", len(left))
	}
	if other, _ := m.GetMyList("u2", ""); len(other) != 1 {
		t.Fatalf("u2 touched: %d entries", len(other))
	}
}

func TestMyList_Capped(t *testing.T) {
	m := NewDBManager(newTestDB(t, MyListSchema))
	for i := 0; i < MyListMax+3; i++ {
		id := string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + string(rune('0'+i/260))
		if err := m.AddToMyList("u", MyListEntry{PluginID: "p", MediaID: id}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := m.GetMyList("u", "")
	if len(got) != MyListMax {
		t.Fatalf("%d entries, want cap %d", len(got), MyListMax)
	}
}

func TestMyList_ExtraRoundTripAndBlankRefresh(t *testing.T) {
	m := NewDBManager(newTestDB(t, MyListSchema))
	full := MyListEntry{PluginID: "p", MediaID: "a", Title: "A", Year: 2020,
		Extra: map[string]string{"plot": "trama", "genres": "Azione, Drama"}}
	if err := m.AddToMyList("u", full); err != nil {
		t.Fatal(err)
	}
	// A partial card (e.g. from a device that only had the id) keeps the rest.
	if err := m.AddToMyList("u", MyListEntry{PluginID: "p", MediaID: "a"}); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetMyList("u", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "A" || got[0].Year != 2020 || got[0].Extra["plot"] != "trama" {
		t.Fatalf("after partial refresh: %+v", got)
	}
}
