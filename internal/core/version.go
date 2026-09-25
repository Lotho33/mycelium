package core

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is the running core version.
// Override at build time: go build -ldflags "-X mycelium/internal/core.Version=v1.2.3"
var Version = "1.5.6"

const githubReleaseAPI = "https://api.github.com/repos/Lotho33/mycelium/releases/latest"

// normalizeVersion strips an optional leading "v"/"V" prefix, so version
// strings from different sources compare correctly regardless of format:
// Version (the running binary) has none by default but can be overridden
// with a "v"-prefixed value via -ldflags, while GitHub release tags are
// always prefixed (e.g. "v1.3.2").
func normalizeVersion(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
}

// SameVersion reports whether a and b denote the same release, ignoring an
// optional "v"/"V" prefix on either side (e.g. "1.3.2" and "v1.3.2" match).
func SameVersion(a, b string) bool {
	return normalizeVersion(a) == normalizeVersion(b)
}

// parseVersionParts splits a normalized "major.minor.patch"-shaped version
// string into up to 3 integer components. Missing trailing components (e.g.
// "1.3" has no patch) are treated as 0. ok is false if any present component
// fails to parse as a non-negative integer — callers must treat that as "we
// don't know the order", never silently fall back to 0 for a malformed part.
func parseVersionParts(s string) (parts [3]int, ok bool) {
	fields := strings.SplitN(s, ".", 3)
	for i, f := range fields {
		if f == "" {
			return parts, false
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}

// CompareVersions compares two "major.minor.patch"-shaped version strings
// (an optional leading "v"/"V" on either side is ignored, and a missing
// minor/patch component is treated as 0), returning -1 if a < b, 0 if they
// denote the same release, and 1 if a > b.
//
// If either string fails to parse (non-numeric component, empty field, …),
// CompareVersions returns 0 and logs a warning — the fail-safe contract lives
// in IsNewerVersion, which never reports "newer" for a comparison it can't
// make sense of; a bare 0 here would otherwise read as "same version" to any
// other caller, which is the correct conservative default too.
func CompareVersions(a, b string) int {
	an, aOK := parseVersionParts(normalizeVersion(a))
	bn, bOK := parseVersionParts(normalizeVersion(b))
	if !aOK || !bOK {
		log.Printf("[core] CompareVersions: formato versione non riconosciuto (a=%q b=%q), tratto come uguali", a, b)
		return 0
	}
	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// IsNewerVersion reports whether candidate is a strictly newer release than
// current (both "major.minor.patch"-shaped, "v"-prefix optional either side).
// It's the fix for a real dashboard bug: comparing versions with SameVersion
// (string equality) meant ANY difference — including the running binary
// already being AHEAD of the latest published GitHub release, e.g. because a
// newer build was deployed before its matching tag/release went out — was
// treated as "not up to date" and offered as an update, even backwards
// (proposing to "update" to an older version than the one already running).
//
// Fail-safe by construction: if either string doesn't parse as a version,
// this returns false rather than risk offering a bogus update — better to
// silently not propose an update than to propose a wrong one.
func IsNewerVersion(candidate, current string) bool {
	return CompareVersions(candidate, current) > 0
}

var (
	latestMu           sync.Mutex
	latestVersion      string
	latestFetched      time.Time
	latestFetching     bool
	latestAutoFetchOff bool
)

// LatestVersion returns the latest published release tag from GitHub.
// Non-blocking: triggers a background fetch on first call and on hourly refresh.
// Returns "" until the first fetch completes.
func LatestVersion() string {
	latestMu.Lock()
	defer latestMu.Unlock()
	if !latestAutoFetchOff && time.Since(latestFetched) >= time.Hour && !latestFetching {
		latestFetching = true
		go func() {
			// Always clears latestFetching, even if fetchGitHubLatestTag panics —
			// otherwise a single bad response permanently wedges the hourly
			// refresh (checked in this defer, so it runs no matter what).
			defer func() {
				latestMu.Lock()
				latestFetching = false
				latestMu.Unlock()
			}()
			defer Guard("core/version-fetch")
			tag := fetchGitHubLatestTag()
			latestMu.Lock()
			latestFetched = time.Now()
			if tag != "" {
				latestVersion = tag
			}
			latestMu.Unlock()
		}()
	}
	return latestVersion
}

func fetchGitHubLatestTag() string {
	client := HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequest("GET", githubReleaseAPI, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mycelium-core/"+normalizeVersion(Version))

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}
	return payload.TagName
}

// SetLatestVersionForTest overrides the cached "latest" version and marks it
// freshly fetched, bypassing the real GitHub call entirely. Test-only — mirrors
// the SetAdminSessionKey seam used elsewhere for injecting test state into an
// otherwise package-private singleton.
func SetLatestVersionForTest(v string) {
	latestMu.Lock()
	latestVersion = v
	latestFetched = time.Now()
	latestMu.Unlock()
}

// DisableAutoFetchForTest permanently stops LatestVersion from ever spawning
// its background GitHub-fetch goroutine, for the remainder of the test
// binary's life. Call it once, before any test runs (e.g. from a TestMain) —
// not per-test. Without this, whichever test happens to be the first to call
// LatestVersion() in the whole binary (directly, or indirectly through any
// handler that calls it, e.g. getAdminInfo/coreUpdate) kicks off a REAL
// network request to api.github.com; that goroutine can complete at any later
// point and overwrite latestVersion out from under an unrelated test that
// called SetLatestVersionForTest for its own scenario in the meantime — a
// real, observed flake (a "latest" value seeded for one test getting
// silently replaced by whatever mycelium-core's actual current release is,
// mid-test). Disabling the fetch entirely removes the race at its root
// instead of trying to out-time it.
func DisableAutoFetchForTest() {
	latestMu.Lock()
	latestAutoFetchOff = true
	latestMu.Unlock()
}

// githubAPIBase is the GitHub API root used by LatestVersionOf. A var (not a
// const like githubReleaseAPI above) purely so tests can point it at an
// httptest server instead of the real GitHub API — see
// SetGitHubAPIBaseForTest. fetchGitHubLatestTag/LatestVersion are untouched
// by this: they keep hitting the real githubReleaseAPI constant.
var githubAPIBase = "https://api.github.com"

// SetGitHubAPIBaseForTest points LatestVersionOf at base for the duration of
// a test and returns a restore func. Test-only seam.
func SetGitHubAPIBaseForTest(base string) func() {
	orig := githubAPIBase
	githubAPIBase = base
	return func() { githubAPIBase = orig }
}

// GitHubReleaseAsset is one asset attached to a GitHub release, as returned
// by the "releases/latest" API — the subset LatestVersionOf callers need to
// pick and download the right one.
type GitHubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// LatestVersionOf fetches the latest published release of an arbitrary
// "owner/repo" GitHub repository and returns its tag plus its full asset
// list. It's the parametric counterpart to LatestVersion (which is hardcoded
// to this repo, cached hourly, and fire-and-forget): here the repo is chosen
// at runtime by the caller (e.g. an admin-configured setting for a *different*
// project's repo), the call is synchronous, and any failure is returned
// directly instead of being swallowed into an empty string — this is meant to
// be invoked on-demand (an admin clicking a button), not polled in the
// background, so there's no cache to serve a stale-but-non-blocking answer
// from.
func LatestVersionOf(repoSlug string) (tag string, assets []GitHubReleaseAsset, err error) {
	client := HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequest("GET", githubAPIBase+"/repos/"+repoSlug+"/releases/latest", nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mycelium-core/"+normalizeVersion(Version))

	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("richiesta GitHub per %s: %w", repoSlug, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("GitHub API %s: status %d", repoSlug, resp.StatusCode)
	}

	var payload struct {
		TagName string               `json:"tag_name"`
		Assets  []GitHubReleaseAsset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", nil, fmt.Errorf("decodifica risposta GitHub per %s: %w", repoSlug, err)
	}
	if payload.TagName == "" {
		return "", nil, fmt.Errorf("nessuna release trovata per %s", repoSlug)
	}
	return payload.TagName, payload.Assets, nil
}
