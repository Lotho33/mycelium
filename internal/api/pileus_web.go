// Pileus web-app updater: POST /admin/pileus-web/update downloads the web
// build asset of the latest release of the "pileus_web_repo" repository
// ("owner/repo") and installs it into data/pileus-web/, served at /app.
package api

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// defaultPileusWebRepo is the "pileus_web_repo" value until the admin sets
// one (shared with getAdminInfo).
const defaultPileusWebRepo = "Lotho33/pileus"

// repoSlugRe validates the "owner/repo" shape of pileus_web_repo before it
// goes into a GitHub API URL.
var repoSlugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// maxPileusWebAssetBytes caps the downloaded release asset.
const maxPileusWebAssetBytes = 100 << 20

// RegisterPileusWebRoutes wires the Pileus web-app updater endpoint onto mux.
func RegisterPileusWebRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/pileus-web/update", adminAuthMiddleware(updatePileusWebApp))
	mux.HandleFunc("GET /admin/pileus-web/releases", adminAuthMiddleware(listPileusWebReleases))
	startPileusWebAutoUpdate()
}

// updatePileusWebApp installs the latest web build of the configured repo,
// or the release named by {"tag": "..."} (a rollback when it is older).
// When mycelium itself is behind its latest release it answers with a
// warning and installs nothing, unless the body says {"force": true}.
func updatePileusWebApp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Force bool   `json:"force"`
		Tag   string `json:"tag"`
	}
	// The body is optional: no body means the latest release, force=false.
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	status, resp := runPileusWebUpdate(body.Force, strings.TrimSpace(body.Tag))
	writeJSON(w, status, resp)
}

// pileusWebReleasesShown is how many recent releases the dashboard offers
// to install (the rollback list).
const pileusWebReleasesShown = 15

// listPileusWebReleases lists the recent releases of the configured repo
// that carry a web build, newest first, for the rollback picker.
func listPileusWebReleases(w http.ResponseWriter, r *http.Request) {
	slug, ok := normalizeRepoSlug(managers.Settings.GetString("pileus_web_repo", defaultPileusWebRepo))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "repository Pileus non valido"})
		return
	}
	rels, err := core.ReleasesOf(slug, pileusWebReleasesShown*2)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"detail": fmt.Sprintf("impossibile leggere le release di %s: %v", slug, err)})
		return
	}
	type item struct {
		Tag         string    `json:"tag"`
		Name        string    `json:"name,omitempty"`
		PublishedAt time.Time `json:"published_at"`
		Latest      bool      `json:"latest"`
	}
	out := []item{}
	for i, rel := range rels {
		if _, ok := pickWebAsset(rel.Assets); !ok {
			continue
		}
		out = append(out, item{Tag: rel.Tag, Name: rel.Name, PublishedAt: rel.PublishedAt, Latest: i == 0})
		if len(out) == pileusWebReleasesShown {
			break
		}
	}
	installed := ""
	if rec := managers.Settings.GetString(pileusWebInstalledTagKey, ""); strings.HasPrefix(rec, slug+"@") {
		installed = strings.TrimPrefix(rec, slug+"@")
	}
	hold := ""
	if h := managers.Settings.GetString(pileusWebHoldTagKey, ""); strings.HasPrefix(h, slug+"@") {
		hold = strings.TrimPrefix(h, slug+"@")
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": slug, "releases": out, "installed": installed, "held": hold})
}

// pileusWebInstalledTagKey remembers the release tag last installed, so the
// automatic check updates only when a new release is out.
const pileusWebInstalledTagKey = "pileus_web_installed_tag"

// pileusWebHoldTagKey ("owner/repo@tag") is the release that was the latest
// when the admin rolled back: the automatic updates skip it, so the faulty
// build doesn't come straight back, and resume with the next release.
const pileusWebHoldTagKey = "pileus_web_hold_tag"

// runPileusWebUpdate installs the latest web build, or the release wantTag
// when set; it answers what the endpoint returns (status, body). Shared by
// the dashboard button and the automatic updates.
func runPileusWebUpdate(force bool, wantTag string) (int, map[string]any) {
	repoSlug := strings.TrimSpace(managers.Settings.GetString("pileus_web_repo", defaultPileusWebRepo))
	if repoSlug == "" {
		return http.StatusBadRequest, map[string]any{
			"detail": "configura il repo Pileus nelle impostazioni (pileus_web_repo, formato owner/repo) prima di aggiornare l'app web",
		}
	}
	// "owner/repo" or the repository's GitHub URL.
	slug, ok := normalizeRepoSlug(repoSlug)
	if !ok {
		return http.StatusBadRequest, map[string]any{
			"detail": "pileus_web_repo deve essere owner/repo o l'indirizzo GitHub del repository: " + repoSlug,
		}
	}
	repoSlug = slug

	if wantTag != "" {
		return installPileusWebTag(repoSlug, wantTag)
	}

	// Compatibility gate: a core behind its latest release may lack something
	// the new web build expects. "" (latest version not fetched yet) counts as
	// unknown, not as outdated.
	latestCore := core.LatestVersion()
	myceliumUpToDate := latestCore == "" || !core.IsNewerVersion(latestCore, core.Version)
	if !myceliumUpToDate && !force {
		return http.StatusOK, map[string]any{
			"ok":                  false,
			"mycelium_up_to_date": false,
			"mycelium_version":    core.Version,
			"mycelium_latest":     latestCore,
			"warning":             "mycelium non è sull'ultima versione — il build web più recente potrebbe assumere funzionalità non ancora presenti in questa versione del server. Riprova con force per procedere comunque.",
		}
	}

	tag, assets, err := core.LatestVersionOf(repoSlug)
	if err != nil {
		return http.StatusBadGateway, map[string]any{
			"detail": fmt.Sprintf("impossibile leggere l'ultima release di %s: %v", repoSlug, err),
		}
	}
	asset, ok := pickWebAsset(assets)
	if !ok {
		return http.StatusNotFound, map[string]any{
			"detail": fmt.Sprintf("nessun asset .zip o .tar.gz che sembri il build web trovato nella release %s di %s", tag, repoSlug),
		}
	}

	// Integrity: when the release ships a SHA256SUMS file the archive must
	// match it; otherwise the update proceeds and the response says so. This
	// catches a corrupted or swapped download, not a compromised release.
	expectedSHA, sumsErr := releaseChecksumFor(assets, asset.Name)
	if sumsErr != nil {
		return http.StatusBadGateway, map[string]any{"detail": "lettura checksum della release: " + sumsErr.Error()}
	}

	destDir := core.AppPath("data", "pileus-web")
	if err := installPileusWebAsset(asset, destDir, expectedSHA); err != nil {
		return http.StatusInternalServerError, map[string]any{"detail": err.Error()}
	}
	// The latest release installed on purpose: nothing is held back any more.
	if err := managers.Settings.SaveInternal(map[string]any{pileusWebInstalledTagKey: repoSlug + "@" + tag, pileusWebHoldTagKey: ""}); err != nil {
		log.Printf("[pileus-web] salvataggio della release installata: %v", err)
	}

	log.Printf("[pileus-web] app web aggiornata a %s (asset %s, checksum verificato: %v) da %s", tag, asset.Name, expectedSHA != "", repoSlug)
	resp := map[string]any{
		"ok":                  true,
		"version":             tag,
		"asset":               asset.Name,
		"checksum_verified":   expectedSHA != "",
		"mycelium_up_to_date": myceliumUpToDate,
	}
	if expectedSHA == "" {
		resp["warning"] = "la release non pubblica un file SHA256SUMS: integrità dell'archivio non verificata"
	}
	return http.StatusOK, resp
}

// installPileusWebTag installs the release tag of repoSlug. When it is not
// the latest (a rollback), the latest is held back from the automatic
// updates until a newer one is out. No compatibility gate: an older build
// expects less of the server, not more.
func installPileusWebTag(repoSlug, wantTag string) (int, map[string]any) {
	rels, err := core.ReleasesOf(repoSlug, 100)
	if err != nil {
		return http.StatusBadGateway, map[string]any{"detail": fmt.Sprintf("impossibile leggere le release di %s: %v", repoSlug, err)}
	}
	idx := -1
	for i, rel := range rels {
		if rel.Tag == wantTag {
			idx = i
			break
		}
	}
	if idx < 0 {
		return http.StatusNotFound, map[string]any{"detail": fmt.Sprintf("release %s non trovata in %s", wantTag, repoSlug)}
	}
	rel := rels[idx]
	asset, ok := pickWebAsset(rel.Assets)
	if !ok {
		return http.StatusNotFound, map[string]any{"detail": fmt.Sprintf("la release %s di %s non contiene il build web", wantTag, repoSlug)}
	}
	expectedSHA, sumsErr := releaseChecksumFor(rel.Assets, asset.Name)
	if sumsErr != nil {
		return http.StatusBadGateway, map[string]any{"detail": "lettura checksum della release: " + sumsErr.Error()}
	}
	if err := installPileusWebAsset(asset, core.AppPath("data", "pileus-web"), expectedSHA); err != nil {
		return http.StatusInternalServerError, map[string]any{"detail": err.Error()}
	}
	hold := ""
	if idx > 0 {
		hold = repoSlug + "@" + rels[0].Tag
	}
	if err := managers.Settings.SaveInternal(map[string]any{pileusWebInstalledTagKey: repoSlug + "@" + rel.Tag, pileusWebHoldTagKey: hold}); err != nil {
		log.Printf("[pileus-web] salvataggio della release installata: %v", err)
	}
	if hold != "" {
		log.Printf("[pileus-web] app web riportata a %s; aggiornamenti automatici sospesi fino a una release più nuova di %s", rel.Tag, rels[0].Tag)
	} else {
		log.Printf("[pileus-web] app web installata alla %s (asset %s)", rel.Tag, asset.Name)
	}
	resp := map[string]any{
		"ok":                  true,
		"version":             rel.Tag,
		"asset":               asset.Name,
		"checksum_verified":   expectedSHA != "",
		"mycelium_up_to_date": true,
		"rolled_back":         hold != "",
	}
	if expectedSHA == "" {
		resp["warning"] = "la release non pubblica un file SHA256SUMS: integrità dell'archivio non verificata"
	}
	return http.StatusOK, resp
}

// ─── Automatic updates ────────────────────────────────────────────────────────

// pileusWebAutoEvery is how often the automatic updates look for a new
// release: one unauthenticated GitHub API call (the limit is 60 an hour per
// IP), so a fix reaches the devices within the hour.
const pileusWebAutoEvery = time.Hour

var pileusWebAutoOnce sync.Once

// startPileusWebAutoUpdate keeps the web app on the latest release of the
// configured repository (setting pileus_web_auto_update, on by default): a
// few minutes after start and then every pileusWebAutoEvery.
func startPileusWebAutoUpdate() {
	pileusWebAutoOnce.Do(func() {
		core.SafeGo("pileus-web/auto-update", func() {
			time.Sleep(2 * time.Minute)
			for {
				autoUpdatePileusWeb()
				time.Sleep(pileusWebAutoEvery)
			}
		})
	})
}

// pileusWebNeedsUpdate decides whether latestTag of repoSlug should be
// installed: not when it is the release installed last; with no record of
// that (an install from before this was tracked), when the installed build
// is missing or older.
func pileusWebNeedsUpdate(repoSlug, latestTag string) bool {
	// Rolled back from this very release: wait for the next one.
	if managers.Settings.GetString(pileusWebHoldTagKey, "") == repoSlug+"@"+latestTag {
		return false
	}
	if rec := managers.Settings.GetString(pileusWebInstalledTagKey, ""); rec != "" {
		return rec != repoSlug+"@"+latestTag
	}
	info, ok := pileusWebVersion()
	if !ok {
		return true
	}
	return core.IsNewerVersion(latestTag, info.Version)
}

func autoUpdatePileusWeb() {
	if !managers.Settings.GetBool("pileus_web_auto_update", true) {
		return
	}
	slug, ok := normalizeRepoSlug(managers.Settings.GetString("pileus_web_repo", defaultPileusWebRepo))
	if !ok {
		return
	}
	tag, _, err := core.LatestVersionOf(slug)
	if err != nil {
		log.Printf("[pileus-web] controllo aggiornamenti di %s non riuscito: %v", slug, err)
		return
	}
	if !pileusWebNeedsUpdate(slug, tag) {
		return
	}
	// Never forced: a server behind its own latest release waits (the
	// dashboard can still force it).
	status, resp := runPileusWebUpdate(false, "")
	switch {
	case status != http.StatusOK:
		log.Printf("[pileus-web] aggiornamento automatico non riuscito: %v", resp["detail"])
	case resp["ok"] == false:
		log.Printf("[pileus-web] aggiornamento automatico rimandato: %v", resp["warning"])
	}
}

// pickWebAsset chooses the web-build asset among a release's assets: the
// lowercased name contains "web" and ends in .zip, .tar.gz or .tgz (so
// GitHub's automatic source archives never match). Names containing "tv"
// are the separate TV build and are skipped. With several matches the
// first wins and a warning is logged.
func pickWebAsset(assets []core.GitHubReleaseAsset) (core.GitHubReleaseAsset, bool) {
	var match core.GitHubReleaseAsset
	found := false
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if !isSupportedWebArchiveName(name) || !strings.Contains(name, "web") {
			continue
		}
		if strings.Contains(name, "tv") {
			continue // TV build, not handled here
		}
		if found {
			log.Printf("[pileus-web] più asset sembrano il build web nella release, uso il primo trovato (%s), ignoro %s", match.Name, a.Name)
			continue
		}
		match = a
		found = true
	}
	return match, found
}

// isSupportedWebArchiveName reports whether lowerName ends in an archive
// extension this endpoint can extract.
func isSupportedWebArchiveName(lowerName string) bool {
	return strings.HasSuffix(lowerName, ".zip") ||
		strings.HasSuffix(lowerName, ".tar.gz") ||
		strings.HasSuffix(lowerName, ".tgz")
}

// isTarGzArchiveName reports whether lowerName is a gzipped tar.
func isTarGzArchiveName(lowerName string) bool {
	return strings.HasSuffix(lowerName, ".tar.gz") || strings.HasSuffix(lowerName, ".tgz")
}

// releaseChecksumFor looks for a SHA256SUMS* asset in the release and
// returns the digest it lists for assetName ("" when absent).
func releaseChecksumFor(assets []core.GitHubReleaseAsset, assetName string) (string, error) {
	for _, a := range assets {
		lower := strings.ToLower(a.Name)
		if !strings.HasPrefix(lower, "sha256sums") || a.BrowserDownloadURL == "" {
			continue
		}
		body, err := fetchSmall(a.BrowserDownloadURL, 64<<10)
		if err != nil {
			return "", err
		}
		if sum := parseSHA256Sums(body, assetName); sum != "" {
			return sum, nil
		}
	}
	return "", nil
}

// parseSHA256Sums finds name in `sha256sum` output ("<hex>  <name>", or
// "<hex> *<name>" in binary mode) and returns its lowercased digest.
func parseSHA256Sums(body []byte, name string) string {
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name && len(fields[0]) == 64 {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

// fetchSmall GETs url and returns at most limit bytes of the body.
func fetchSmall(url string, limit int64) ([]byte, error) {
	client := core.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mycelium-core/"+core.Version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// installPileusWebAsset downloads the archive, verifies it against
// expectedSHA (when set), extracts it with the safe extractors into a
// sibling temp directory, checks it contains an index.html, then swaps it
// in: destDir becomes destDir.bak (one backup slot) and the extraction
// becomes destDir.
func installPileusWebAsset(asset core.GitHubReleaseAsset, destDir, expectedSHA string) error {
	if asset.BrowserDownloadURL == "" {
		return fmt.Errorf("asset senza browser_download_url")
	}

	// The shared client's 20 s total timeout is for API calls: an archive of
	// tens of MB on a slow link needs longer.
	client := &http.Client{Timeout: 10 * time.Minute}
	if core.HTTPClient != nil {
		client.Transport = core.HTTPClient.Transport
	}
	req, err := http.NewRequest("GET", asset.BrowserDownloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mycelium-core/"+core.Version)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download asset: status %d", resp.StatusCode)
	}

	lowerName := strings.ToLower(asset.Name)
	isTarGz := isTarGzArchiveName(lowerName)
	tmpExt := ".zip"
	if isTarGz {
		tmpExt = ".tar.gz"
	}

	tmpArchive, err := os.CreateTemp("", "pileus_web_*"+tmpExt)
	if err != nil {
		return err
	}
	tmpArchivePath := tmpArchive.Name()
	defer os.Remove(tmpArchivePath)

	// Read one byte past the cap to detect an oversized asset.
	n, err := io.Copy(tmpArchive, io.LimitReader(resp.Body, maxPileusWebAssetBytes+1))
	tmpArchive.Close()
	if err != nil {
		return fmt.Errorf("scrittura archivio temporaneo: %w", err)
	}
	if n > maxPileusWebAssetBytes {
		return fmt.Errorf("asset troppo grande (oltre %d MiB)", maxPileusWebAssetBytes>>20)
	}
	if expectedSHA != "" {
		got, err := fileSHA256(tmpArchivePath)
		if err != nil {
			return fmt.Errorf("calcolo checksum: %w", err)
		}
		if got != expectedSHA {
			return fmt.Errorf("checksum dell'archivio non corrisponde a SHA256SUMS della release (atteso %s, ottenuto %s): aggiornamento annullato", expectedSHA, got)
		}
	}

	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		return fmt.Errorf("creazione directory data/: %w", err)
	}
	extractDir, err := os.MkdirTemp(filepath.Dir(destDir), "pileus-web-extract-*")
	if err != nil {
		return err
	}
	removeExtractDir := true
	defer func() {
		if removeExtractDir {
			os.RemoveAll(extractDir)
		}
	}()

	// Caps sized for a Flutter web build.
	const maxPerFile = 30 << 20 // 30 MiB
	const maxTotal = 150 << 20  // 150 MiB

	var aborted bool
	if isTarGz {
		aborted, _, err = extractTarGzSafe(tmpArchivePath, extractDir, maxPerFile, maxTotal, "[pileus-web]")
		if err != nil {
			return fmt.Errorf("archivio tar.gz non valido: %w", err)
		}
	} else {
		zr, err := zip.OpenReader(tmpArchivePath)
		if err != nil {
			return fmt.Errorf("ZIP corrotto: %w", err)
		}
		defer zr.Close()
		aborted, _, err = extractZipSafe(&zr.Reader, extractDir, maxPerFile, maxTotal, "[pileus-web]")
		if err != nil {
			return fmt.Errorf("archivio ZIP non valido: %w", err)
		}
	}
	if aborted {
		return fmt.Errorf("archivio troppo grande una volta estratto")
	}

	root, ok := webRoot(extractDir)
	if !ok {
		return fmt.Errorf("l'archivio non contiene un index.html: non sembra un build web Flutter valido")
	}

	bakDir := destDir + ".bak"
	if err := os.RemoveAll(bakDir); err != nil {
		// Typically a backup left by a root-run server, which the unprivileged
		// one can't empty: move it aside (only the parent's write bit counts)
		// rather than fail the update with rename's opaque "file exists".
		stale := fmt.Sprintf("%s.stale-%d", bakDir, time.Now().Unix())
		if rerr := os.Rename(bakDir, stale); rerr != nil {
			return fmt.Errorf("rimozione del backup precedente %s fallita: %v", bakDir, err)
		}
		log.Printf("[pileus-web] backup precedente non rimovibile (%v): spostato in %s", err, stale)
	}
	if _, err := os.Stat(destDir); err == nil {
		if err := os.Rename(destDir, bakDir); err != nil {
			return fmt.Errorf("backup versione precedente fallito: %w", err)
		}
	}
	if err := os.Rename(root, destDir); err != nil {
		// Best-effort: restore the previous version.
		if _, statErr := os.Stat(destDir); statErr != nil {
			os.Rename(bakDir, destDir)
		}
		return fmt.Errorf("installazione fallita: %w", err)
	}
	// A build one level down leaves the (now empty) extraction dir behind.
	removeExtractDir = root != extractDir
	return nil
}

// fileSHA256 returns the lowercase hex SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// webRoot finds the build inside an extracted archive: dir itself when it
// holds index.html, else its single subdirectory that does (archives often
// wrap the build in a "web/" folder).
func webRoot(dir string) (string, bool) {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		return dir, true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	found := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "index.html")); err == nil {
			if found != "" {
				return "", false // ambiguous
			}
			found = filepath.Join(dir, e.Name())
		}
	}
	return found, found != ""
}
