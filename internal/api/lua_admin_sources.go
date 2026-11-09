package api

// Plugins installed from an address instead of an uploaded ZIP, and kept up
// to date from it.
//
// A source is either a GitHub repository ("owner/repo" or its github.com
// URL) or a direct link to a .zip. For a repository the ZIP assets of its
// latest release are used; a repository without releases is installed from
// the latest commit of its default branch. Each plugin remembers where it
// came from (setting "plugin_sources", plugin id → source) and, when the
// admin turns it on, is updated automatically every few hours.
//
// Every install goes through installPluginZip, the same path as an upload:
// the installed copy stays until the new one loads. Before an update the
// archive's manifest is checked to hold that same plugin, so a source that
// starts shipping something else can't replace it.

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"mycelium/internal/core"
	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// pluginSource is where an installed plugin came from.
type pluginSource struct {
	Source     string `json:"source"`          // "owner/repo" or a .zip URL
	Asset      string `json:"asset,omitempty"` // release asset name, for repos shipping several plugins
	AutoUpdate bool   `json:"auto_update"`
	Version    string `json:"version,omitempty"` // last installed: release tag, commit, or the ZIP's sha256
	ETag       string `json:"etag,omitempty"`    // direct links: to ask "changed?" without downloading
	CheckedAt  int64  `json:"checked_at,omitempty"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

const (
	pluginSourcesKey      = "plugin_sources"
	maxPluginDownload     = 64 << 20
	pluginDownloadTimeout = 3 * time.Minute
	pluginAutoUpdateEvery = 6 * time.Hour
)

// checkSourceURL refuses downloads from internal addresses (a var: tests
// serve archives from loopback).
var checkSourceURL = core.CheckURLNotSSRF

var (
	sourcesMu sync.Mutex
	// checkMu serialises checks: an automatic round and a click on "check now"
	// must not install the same plugin twice at once.
	checkMu sync.Mutex
)

func loadPluginSources() map[string]pluginSource {
	out := map[string]pluginSource{}
	if raw := managers.Settings.GetString(pluginSourcesKey, ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			log.Printf("[plugins/source] %s illeggibile, ignorato: %v", pluginSourcesKey, err)
			return map[string]pluginSource{}
		}
	}
	return out
}

func savePluginSources(m map[string]pluginSource) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return managers.Settings.SaveInternal(map[string]any{pluginSourcesKey: string(raw)})
}

// updatePluginSource applies fn to pid's entry (created when missing) and
// saves. fn returning false deletes the entry.
func updatePluginSource(pid string, fn func(*pluginSource) bool) error {
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	m := loadPluginSources()
	s := m[pid]
	if fn(&s) {
		m[pid] = s
	} else {
		delete(m, pid)
	}
	return savePluginSources(m)
}

// forgetPluginSource drops pid's source (the plugin was uninstalled).
func forgetPluginSource(pid string) {
	if err := updatePluginSource(pid, func(*pluginSource) bool { return false }); err != nil {
		log.Printf("[plugins/source] %s: %v", pid, err)
	}
}

// ─── Parsing ──────────────────────────────────────────────────────────────────

// parsedSource is a source in canonical form: a GitHub slug or a URL.
type parsedSource struct {
	Repo string // "owner/repo"
	URL  string // direct .zip link
}

func (p parsedSource) String() string {
	if p.Repo != "" {
		return p.Repo
	}
	return p.URL
}

var errBadSource = errors.New("indica owner/repo, l'indirizzo di un repository GitHub o il link diretto a un file .zip")

// normalizeRepoSlug turns "owner/repo", "github.com/owner/repo" or any
// github.com URL of the repository (.git, /releases, /tree/... included)
// into "owner/repo".
func normalizeRepoSlug(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if repoSlugRe.MatchString(strings.TrimSuffix(s, ".git")) {
		return strings.TrimSuffix(s, ".git"), true
	}
	raw := s
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", false
	}
	slug := parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
	if !repoSlugRe.MatchString(slug) {
		return "", false
	}
	return slug, true
}

func parsePluginSource(s string) (parsedSource, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return parsedSource{}, errBadSource
	}
	u, uerr := url.Parse(s)
	// A link to a file is a direct download even on github.com (a release asset).
	if uerr == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.HasSuffix(strings.ToLower(u.Path), ".zip") {
		return parsedSource{URL: u.String()}, nil
	}
	if slug, ok := normalizeRepoSlug(s); ok {
		return parsedSource{Repo: slug}, nil
	}
	return parsedSource{}, errBadSource
}

// ─── Resolving and downloading ────────────────────────────────────────────────

// candidate is one installable archive offered by a source.
type candidate struct {
	Name    string // release asset name ("" for a source archive or a direct link)
	URL     string
	Version string // known before downloading (release tag, commit); "" for direct links
}

// resolveSource lists what a source offers right now.
func resolveSource(ctx context.Context, p parsedSource) ([]candidate, error) {
	if p.URL != "" {
		return []candidate{{URL: p.URL}}, nil
	}
	tag, assets, err := core.LatestVersionOf(p.Repo)
	if err != nil {
		if !strings.Contains(err.Error(), "status 404") && !strings.Contains(err.Error(), "nessuna release") {
			return nil, err
		}
		// No releases: the latest commit of the default branch.
		sha, berr := latestCommit(ctx, p.Repo)
		if berr != nil {
			return nil, fmt.Errorf("%s non ha release e non è stato possibile leggere il ramo principale: %w", p.Repo, berr)
		}
		return []candidate{{URL: core.GitHubAPIBase() + "/repos/" + p.Repo + "/zipball/" + sha, Version: sha[:min(12, len(sha))]}}, nil
	}
	var out []candidate
	for _, a := range assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".zip") && a.BrowserDownloadURL != "" {
			out = append(out, candidate{Name: a.Name, URL: a.BrowserDownloadURL, Version: tag})
		}
	}
	if len(out) == 0 {
		// A release without ZIP assets: its source archive.
		out = append(out, candidate{URL: core.GitHubAPIBase() + "/repos/" + p.Repo + "/zipball/" + url.PathEscape(tag), Version: tag})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func latestCommit(ctx context.Context, slug string) (string, error) {
	body, err := githubAPIGet(ctx, "/repos/"+slug)
	if err != nil {
		return "", err
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &repo); err != nil || repo.DefaultBranch == "" {
		return "", fmt.Errorf("repository %s non trovato", slug)
	}
	body, err = githubAPIGet(ctx, "/repos/"+slug+"/commits/"+url.PathEscape(repo.DefaultBranch))
	if err != nil {
		return "", err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &commit); err != nil || commit.SHA == "" {
		return "", fmt.Errorf("ultimo commit di %s illeggibile", slug)
	}
	return commit.SHA, nil
}

func githubAPIGet(ctx context.Context, p string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", core.GitHubAPIBase()+p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mycelium-core/"+core.Version)
	resp, err := sourceHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// sourceHTTPClient downloads from sources: it refuses internal addresses,
// redirects included (a release asset redirects to GitHub's storage).
func sourceHTTPClient() *http.Client {
	c := &http.Client{
		Timeout: pluginDownloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("troppi redirect")
			}
			return checkSourceURL(req.Context(), req.URL.String())
		},
	}
	if core.HTTPClient != nil {
		c.Transport = core.HTTPClient.Transport
	}
	return c
}

// downloaded is a fetched archive on disk; the caller removes Path.
type downloaded struct {
	Path        string
	SHA256      string
	ETag        string
	NotModified bool
}

// downloadArchive fetches rawURL (at most maxPluginDownload bytes) into a
// temp file. With etag set, an unchanged file answers NotModified.
func downloadArchive(ctx context.Context, rawURL, etag string) (downloaded, error) {
	if err := checkSourceURL(ctx, rawURL); err != nil {
		return downloaded{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return downloaded{}, err
	}
	req.Header.Set("User-Agent", "mycelium-core/"+core.Version)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := sourceHTTPClient().Do(req)
	if err != nil {
		return downloaded{}, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return downloaded{NotModified: true, ETag: etag}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return downloaded{}, fmt.Errorf("download: status %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "plugin_src_*.zip")
	if err != nil {
		return downloaded{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxPluginDownload+1))
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return downloaded{}, fmt.Errorf("download: %w", err)
	}
	if n > maxPluginDownload {
		os.Remove(tmp.Name())
		return downloaded{}, fmt.Errorf("archivio oltre %d MB", maxPluginDownload>>20)
	}
	return downloaded{Path: tmp.Name(), SHA256: hex.EncodeToString(h.Sum(nil)), ETag: resp.Header.Get("ETag")}, nil
}

// zipPluginID reads the plugin id from the archive's manifest.yaml (the
// shallowest one), without installing anything.
func zipPluginID(zipPath string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", errors.New("ZIP corrotto")
	}
	defer zr.Close()
	var best *zip.File
	for _, f := range zr.File {
		if path.Base(f.Name) != "manifest.yaml" {
			continue
		}
		if best == nil || strings.Count(f.Name, "/") < strings.Count(best.Name, "/") {
			best = f
		}
	}
	if best == nil {
		return "", errors.New("l'archivio non contiene manifest.yaml")
	}
	rc, err := best.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var mf struct {
		ID string `yaml:"id"`
	}
	if err := yaml.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&mf); err != nil || mf.ID == "" {
		return "", errors.New("manifest.yaml senza id")
	}
	return mf.ID, nil
}

// ─── Install and update ───────────────────────────────────────────────────────

type installedFromSource struct {
	ID      string `json:"id"`
	Update  bool   `json:"update"`
	Version string `json:"version"`
}

// installFromSource installs every archive the source offers (a repository
// can ship several plugins) and remembers the source for each.
func installFromSource(ctx context.Context, p parsedSource, autoUpdate bool) ([]installedFromSource, []string) {
	cands, err := resolveSource(ctx, p)
	if err != nil {
		return nil, []string{err.Error()}
	}
	var done []installedFromSource
	var errs []string
	for _, c := range cands {
		label := c.Name
		if label == "" {
			label = p.String()
		}
		dl, err := downloadArchive(ctx, c.URL, "")
		if err != nil {
			errs = append(errs, label+": "+err.Error())
			continue
		}
		res, ierr := installPluginZip(dl.Path)
		os.Remove(dl.Path)
		if ierr != nil {
			errs = append(errs, label+": "+ierr.Error())
			continue
		}
		version := c.Version
		if version == "" {
			version = dl.SHA256
		}
		now := time.Now().Unix()
		if err := updatePluginSource(res.ID, func(s *pluginSource) bool {
			*s = pluginSource{Source: p.String(), Asset: c.Name, AutoUpdate: autoUpdate, Version: version, ETag: dl.ETag, CheckedAt: now, UpdatedAt: now}
			return true
		}); err != nil {
			log.Printf("[plugins/source] %s: salvataggio origine: %v", res.ID, err)
		}
		log.Printf("[plugins/source] %s installato da %s (%s)", res.ID, p, shortVersion(version))
		done = append(done, installedFromSource{ID: res.ID, Update: res.Update, Version: res.Version})
	}
	return done, errs
}

// checkPluginSource updates pid from its source when something newer is
// there. It reports whether an update was installed.
func checkPluginSource(ctx context.Context, pid string) (updated bool, err error) {
	checkMu.Lock()
	defer checkMu.Unlock()

	src, ok := loadPluginSources()[pid]
	if !ok {
		return false, errors.New("questo plugin non ha un'origine")
	}
	updated, newVersion, etag, err := runSourceCheck(ctx, pid, src)
	now := time.Now().Unix()
	_ = updatePluginSource(pid, func(s *pluginSource) bool {
		if s.Source == "" {
			return false // forgotten meanwhile
		}
		s.CheckedAt = now
		s.LastError = ""
		if err != nil {
			s.LastError = err.Error()
			return true
		}
		if etag != "" {
			s.ETag = etag
		}
		if newVersion != "" {
			s.Version = newVersion
		}
		if updated {
			s.UpdatedAt = now
		}
		return true
	})
	return updated, err
}

func runSourceCheck(ctx context.Context, pid string, src pluginSource) (updated bool, version, etag string, err error) {
	p, err := parsePluginSource(src.Source)
	if err != nil {
		return false, "", "", err
	}
	cands, err := resolveSource(ctx, p)
	if err != nil {
		return false, "", "", err
	}
	c, err := pickCandidate(cands, pid, src.Asset)
	if err != nil {
		return false, "", "", err
	}
	if c.Version != "" && c.Version == src.Version && engine.LuaPlugins.Has(pid) {
		return false, "", "", nil
	}
	ifNoneMatch := ""
	if p.URL != "" {
		ifNoneMatch = src.ETag
	}
	dl, err := downloadArchive(ctx, c.URL, ifNoneMatch)
	if err != nil {
		return false, "", "", err
	}
	if dl.NotModified {
		return false, "", "", nil
	}
	defer os.Remove(dl.Path)
	version = c.Version
	if version == "" {
		version = dl.SHA256
		if version == src.Version && engine.LuaPlugins.Has(pid) {
			return false, "", dl.ETag, nil
		}
	}
	id, err := zipPluginID(dl.Path)
	if err != nil {
		return false, "", "", err
	}
	if id != pid {
		return false, "", "", fmt.Errorf("l'archivio ora contiene il plugin %q, non %q: aggiornamento annullato", id, pid)
	}
	if _, ierr := installPluginZip(dl.Path); ierr != nil {
		return false, "", "", ierr
	}
	log.Printf("[plugins/source] %s aggiornato da %s (%s)", pid, src.Source, shortVersion(version))
	return true, version, dl.ETag, nil
}

// pickCandidate chooses pid's archive among what a source offers: the asset
// it was installed from, the only one, or the one named after the plugin.
func pickCandidate(cands []candidate, pid, asset string) (candidate, error) {
	if len(cands) == 1 {
		return cands[0], nil
	}
	if asset != "" {
		for _, c := range cands {
			if c.Name == asset {
				return c, nil
			}
		}
	}
	var match []candidate
	for _, c := range cands {
		if strings.Contains(strings.ToLower(c.Name), strings.ToLower(pid)) {
			match = append(match, c)
		}
	}
	if len(match) == 1 {
		return match[0], nil
	}
	if len(cands) == 0 {
		return candidate{}, errors.New("la release non contiene archivi .zip")
	}
	return candidate{}, fmt.Errorf("la release contiene %d archivi e nessuno è riconoscibile come %q", len(cands), pid)
}

func shortVersion(v string) string {
	if len(v) > 16 {
		return v[:12]
	}
	return v
}

// ─── Automatic updates ────────────────────────────────────────────────────────

var autoUpdateOnce sync.Once

// startPluginAutoUpdate checks the sources with automatic updates on, a few
// minutes after start and then every pluginAutoUpdateEvery.
func startPluginAutoUpdate() {
	autoUpdateOnce.Do(func() {
		core.SafeGo("plugins/auto-update", func() {
			time.Sleep(3 * time.Minute)
			for {
				runAutoUpdates()
				time.Sleep(pluginAutoUpdateEvery)
			}
		})
	})
}

func runAutoUpdates() {
	for pid, src := range loadPluginSources() {
		if !src.AutoUpdate {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), pluginDownloadTimeout+time.Minute)
		updated, err := checkPluginSource(ctx, pid)
		cancel()
		switch {
		case err != nil:
			log.Printf("[plugins/source] aggiornamento automatico di %s non riuscito: %v", pid, err)
		case updated:
			adminEvents.Poke()
		}
	}
}

// ─── HTTP ─────────────────────────────────────────────────────────────────────

// POST /admin/lua-plugins/install-url {source, auto_update}
func installPluginFromURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source     string `json:"source"`
		AutoUpdate bool   `json:"auto_update"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "JSON non valido"})
		return
	}
	p, err := parsePluginSource(body.Source)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pluginDownloadTimeout+time.Minute)
	defer cancel()
	done, errs := installFromSource(ctx, p, body.AutoUpdate)
	if len(done) == 0 {
		detail := "installazione non riuscita"
		if len(errs) > 0 {
			detail = errs[0]
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"detail": detail, "errors": errs})
		return
	}
	if done == nil {
		done = []installedFromSource{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"installed": done, "errors": errs})
}

// GET /admin/lua-plugins/sources → {sources: {plugin_id: source}}
func listPluginSources(w http.ResponseWriter, r *http.Request) {
	sourcesMu.Lock()
	m := loadPluginSources()
	sourcesMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"sources": m})
}

// POST /admin/lua-plugins/sources/{plugin_id} {source?, auto_update?}
// Links an installed plugin to a source, or changes its automatic updates.
func setPluginSource(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("plugin_id")
	if !engine.LuaPlugins.Has(pid) {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "plugin non trovato"})
		return
	}
	var body struct {
		Source     *string `json:"source"`
		AutoUpdate *bool   `json:"auto_update"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "JSON non valido"})
		return
	}
	var parsed parsedSource
	if body.Source != nil {
		p, err := parsePluginSource(*body.Source)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		parsed = p
	}
	missing := false
	err := updatePluginSource(pid, func(s *pluginSource) bool {
		if body.Source != nil && parsed.String() != s.Source {
			// A new source: nothing known about it yet, the next check installs
			// what it offers.
			*s = pluginSource{Source: parsed.String(), AutoUpdate: s.AutoUpdate}
		}
		if s.Source == "" {
			missing = true
			return false
		}
		if body.AutoUpdate != nil {
			s.AutoUpdate = *body.AutoUpdate
		}
		return true
	})
	if missing {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "indica prima l'origine del plugin"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// DELETE /admin/lua-plugins/sources/{plugin_id}
func deletePluginSource(w http.ResponseWriter, r *http.Request) {
	forgetPluginSource(r.PathValue("plugin_id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// POST /admin/lua-plugins/sources/{plugin_id}/check → {updated, source}
func checkPluginSourceNow(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("plugin_id")
	ctx, cancel := context.WithTimeout(r.Context(), pluginDownloadTimeout+time.Minute)
	defer cancel()
	updated, err := checkPluginSource(ctx, pid)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"detail": err.Error()})
		return
	}
	sourcesMu.Lock()
	src := loadPluginSources()[pid]
	sourcesMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "source": src})
}
