package pileus

import (
	"encoding/json"
	"testing"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
)

func decodeMeta(t *testing.T, js string) luaMeta {
	t.Helper()
	var lm luaMeta
	if err := json.Unmarshal([]byte(js), &lm); err != nil {
		t.Fatalf("decode %s: %v", js, err)
	}
	return lm
}

func TestCardRankAndBadge(t *testing.T) {
	cases := []struct {
		js    string
		rank  int32
		badge string
	}{
		{`{"id":"a","rank":3,"badge":"new"}`, 3, "new"},
		{`{"id":"a","rank":"7","badge":" New_Episode "}`, 7, "new_episode"},
		{`{"id":"a","rank":2.0}`, 2, ""},
		{`{"id":"a","rank":-4}`, 0, ""},
		{`{"id":"a","rank":{"x":1},"badge":"un testo troppo lungo per essere un badge"}`, 0, ""},
		{`{"id":"a"}`, 0, ""},
	}
	for _, c := range cases {
		it := luaMetaToCatalogItem(decodeMeta(t, c.js))
		if it.Rank != c.rank || it.Badge != c.badge {
			t.Errorf("%s: rank %d badge %q, want %d %q", c.js, it.Rank, it.Badge, c.rank, c.badge)
		}
	}
}

func TestSeriesLatestEpisode(t *testing.T) {
	series := func(js string) *gen.SeriesDetails {
		resp := &gen.DetailsResponse{}
		populateDetails(resp, decodeMeta(t, js))
		return resp.GetSeries()
	}
	sd := series(`{"id":"s","media_type":"series","latest_episode":{"season_number":2,"episode_number":8,"air_date":"2026-11-20"}}`)
	if le := sd.GetLatestEpisode(); le == nil || le.SeasonNumber != 2 || le.EpisodeNumber != 8 || le.AirDate != "2026-11-20" {
		t.Errorf("latest episode = %v", le)
	}
	// An empty Lua table serialises as []: no latest episode, no failure.
	for _, js := range []string{
		`{"id":"s","media_type":"series","latest_episode":[]}`,
		`{"id":"s","media_type":"series","latest_episode":{"season_number":1}}`,
		`{"id":"s","media_type":"series"}`,
	} {
		if le := series(js).GetLatestEpisode(); le != nil {
			t.Errorf("%s: latest episode = %v, want none", js, le)
		}
	}
}
