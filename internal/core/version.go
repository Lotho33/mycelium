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

// Version is the running core version, set at build time:
// go build -ldflags "-X mycelium/internal/core.Version=v1.2.3"
var Version = "1.6.7"

const githubReleaseAPI = "https://api.github.com/repos/Lotho33/mycelium/releases/latest"

// normalizeVersion strips an optional leading "v"/"V".
func normalizeVersion(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
}

// SameVersion reports whether a and b denote the same release, ignoring an
// optional "v"/"V" prefix on either side (e.g. "1.3.2" and "v1.3.2" match).
func SameVersion(a, b string) bool {
	return normalizeVersion(a) == normalizeVersion(b)
}

// parseVersionParts splits a normalized "major.minor.patch" string into
// integers (missing parts are 0). ok is false if a present part doesn't
// parse: the order is then unknown.
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

// CompareVersions compares two "major.minor.patch" versions ("v" prefix
// optional, missing parts 0): -1, 0 or 1. An unparsable one yields 0 and a
// warning.
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
// current. False when either doesn't parse: better no update offered than a
// wrong one.
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

// LatestVersion returns the latest release tag of this repository on
// GitHub, fetched in the background (on first call, then hourly); "" until
// the first fetch completes.
func LatestVersion() string {
	latestMu.Lock()
	defer latestMu.Unlock()
	if !latestAutoFetchOff && time.Since(latestFetched) >= time.Hour && !latestFetching {
		latestFetching = true
		go func() {
			// Always clear latestFetching, even on panic, or the refresh would stop.
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

// SetLatestVersionForTest sets the cached latest version (tests only).
func SetLatestVersionForTest(v string) {
	latestMu.Lock()
	latestVersion = v
	latestFetched = time.Now()
	latestMu.Unlock()
}

// DisableAutoFetchForTest stops LatestVersion from ever fetching from GitHub
// (call once, e.g. from TestMain), so a background fetch can't overwrite a
// value a test seeded.
func DisableAutoFetchForTest() {
	latestMu.Lock()
	latestAutoFetchOff = true
	latestMu.Unlock()
}

// githubAPIBase is the GitHub API root used by LatestVersionOf (a var for
// tests).
var githubAPIBase = "https://api.github.com"

// SetGitHubAPIBaseForTest points LatestVersionOf at base and returns a
// restore func (tests only).
func SetGitHubAPIBaseForTest(base string) func() {
	orig := githubAPIBase
	githubAPIBase = base
	return func() { githubAPIBase = orig }
}

// GitHubReleaseAsset is one asset of a GitHub release.
type GitHubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// LatestVersionOf fetches the latest release of an "owner/repo" repository
// and returns its tag and assets. Synchronous and uncached, errors returned:
// meant for on-demand calls.
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
