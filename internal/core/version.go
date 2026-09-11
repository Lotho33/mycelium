package core

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Version is the running core version.
// Override at build time: go build -ldflags "-X mycelium/internal/core.Version=v1.2.3"
var Version = "1.3.0"

const githubReleaseAPI = "https://api.github.com/repos/Lotho33/mycelium/releases/latest"

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
	req.Header.Set("User-Agent", "mycelium-core/"+strings.TrimPrefix(Version, "v"))

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
