package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNormalizeVersion covers the "v"/"V" prefix stripping that lets version
// strings from different sources (build-time Version, GitHub release tags)
// compare correctly regardless of format.
func TestNormalizeVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v1.3.2", "1.3.2"},
		{"V1.3.2", "1.3.2"},
		{"1.3.2", "1.3.2"},
		{"", ""},
		{"v", ""},
	}
	for _, c := range cases {
		if got := normalizeVersion(c.in); got != c.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSameVersion is the regression test for the dashboard bug: GitHub tags
// are always "v"-prefixed (e.g. "v1.3.2") while core.Version isn't, so a bare
// string comparison always reported an update was available even when the
// running binary was already on the latest release.
func TestSameVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.3.2", "1.3.2", true},   // GitHub tag vs. running Version — the reported bug
		{"1.3.2", "v1.3.2", true},   // order shouldn't matter
		{"v1.3.2", "v1.3.2", true},  // both prefixed
		{"1.3.2", "1.3.2", true},    // neither prefixed
		{"v1.3.2", "v1.3.3", false}, // genuinely different releases
		{"1.3.2", "1.3.3", false},
		{"v1.3.2", "1.3.3", false},
	}
	for _, c := range cases {
		if got := SameVersion(c.a, c.b); got != c.want {
			t.Errorf("SameVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestIsNewerVersion is the regression test for the real dashboard bug:
// SameVersion (string equality) treated ANY difference as "update available",
// including the running binary already being AHEAD of the latest published
// GitHub release — offering a backwards "update" to an older version.
// IsNewerVersion must report true only when candidate is strictly newer.
func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		// Exact scenario reported by the user: running v1.3.3, GitHub still
		// serving v1.3.2 (e.g. build deployed before the matching release was
		// published) — 1.3.2 is NOT newer than 1.3.3, so no banner/update.
		{"v1.3.2", "1.3.3", false},
		// Normal case: a real update is available.
		{"1.3.3", "1.3.2", true},
		// Identical after normalization → not "newer", they're the same.
		{"1.3.2", "1.3.2", false},
		{"v1.3.2", "1.3.2", false},
		{"1.3.2", "v1.3.2", false},
		// Shorter formats: missing components treated as 0.
		{"1.4", "1.3.9", true},
		{"1.3", "1.3.0", false},
		{"2", "1.9.9", true},
		{"1.3.0", "1.3", false},
		// Malformed input on either side must never report "newer" — fail-safe.
		{"not-a-version", "1.3.2", false},
		{"1.3.2", "not-a-version", false},
		{"1.x.2", "1.3.2", false},
		{"", "1.3.2", false},
		{"1.3.2", "", false},
		{"1..2", "1.0.2", false},
	}
	for _, c := range cases {
		if got := IsNewerVersion(c.candidate, c.current); got != c.want {
			t.Errorf("IsNewerVersion(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}

// TestCompareVersions covers the -1/0/1 ordering IsNewerVersion derives from.
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.3.2", "1.3.3", -1},
		{"1.3.3", "1.3.2", 1},
		{"1.3.2", "1.3.2", 0},
		{"v1.3.2", "1.3.2", 0},
		{"2.0.0", "1.9.9", 1},
		{"1.9.9", "2.0.0", -1},
		{"bogus", "1.0.0", 0}, // malformed → treated as "equal", never a direction
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestLatestVersionOf_ParsesTagAndPicksAsset points LatestVersionOf at a
// fake GitHub API (httptest) serving a release with several assets — a
// source-code archive, a TV-flavor build and the mobile web build — and
// checks it parses the tag and returns every asset (picking the right one
// among them is pickWebAsset's job, in package api, exercised there).
func TestLatestVersionOf_ParsesTagAndPicksAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/Lotho33/pileus/releases/latest" {
			t.Errorf("unexpected request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v2.4.0",
			"assets": []map[string]any{
				{"name": "Source code.zip", "browser_download_url": "https://example.invalid/src.zip", "size": 123},
				{"name": "pileus-web-tv.zip", "browser_download_url": "https://example.invalid/tv.zip", "size": 456},
				{"name": "pileus-web.zip", "browser_download_url": "https://example.invalid/web.zip", "size": 789},
			},
		})
	}))
	defer srv.Close()
	defer SetGitHubAPIBaseForTest(srv.URL)()

	tag, assets, err := LatestVersionOf("Lotho33/pileus")
	if err != nil {
		t.Fatalf("LatestVersionOf error: %v", err)
	}
	if tag != "v2.4.0" {
		t.Errorf("tag = %q, want %q", tag, "v2.4.0")
	}
	if len(assets) != 3 {
		t.Fatalf("assets = %d, want 3", len(assets))
	}
	names := map[string]string{}
	for _, a := range assets {
		names[a.Name] = a.BrowserDownloadURL
	}
	if names["pileus-web.zip"] != "https://example.invalid/web.zip" {
		t.Errorf("pileus-web.zip URL = %q, want %q", names["pileus-web.zip"], "https://example.invalid/web.zip")
	}
}

// TestLatestVersionOf_ErrorsOnNon200 covers the explicit-error contract that
// distinguishes LatestVersionOf from LatestVersion's fire-and-forget "" —
// callers (the Pileus web-app updater) need to surface a real error message.
func TestLatestVersionOf_ErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	defer SetGitHubAPIBaseForTest(srv.URL)()

	tag, assets, err := LatestVersionOf("Lotho33/does-not-exist")
	if err == nil {
		t.Fatalf("expected an error for a 404 response, got tag=%q assets=%v", tag, assets)
	}
}
