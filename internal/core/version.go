package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Version is the running core version.
// Override at build time: go build -ldflags "-X mycelium/internal/core.Version=v1.2.3"
var Version = "1.3.3"

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

var (
	latestMu       sync.Mutex
	latestVersion  string
	latestFetched  time.Time
	latestFetching bool
)

// LatestVersion returns the latest published release tag from GitHub.
// Non-blocking: triggers a background fetch on first call and on hourly refresh.
// Returns "" until the first fetch completes.
func LatestVersion() string {
	latestMu.Lock()
	defer latestMu.Unlock()
	if time.Since(latestFetched) >= time.Hour && !latestFetching {
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
