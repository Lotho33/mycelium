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
}

// updatePileusWebApp installs the latest web build of the configured repo.
// When mycelium itself is behind its latest release it answers with a
// warning and installs nothing, unless the body says {"force": true}.
func updatePileusWebApp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Force bool `json:"force"`
	}
	// The body is optional: no body means force=false.
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	repoSlug := strings.TrimSpace(managers.Settings.GetString("pileus_web_repo", defaultPileusWebRepo))
	if repoSlug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"detail": "configura il repo Pileus nelle impostazioni (pileus_web_repo, formato owner/repo) prima di aggiornare l'app web",
		})
		return
	}
	if !repoSlugRe.MatchString(repoSlug) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"detail": "pileus_web_repo non è nel formato owner/repo: " + repoSlug,
		})
		return
	}

	// Compatibility gate: a core behind its latest release may lack something
	// the new web build expects. "" (latest version not fetched yet) counts as
	// unknown, not as outdated.
	latestCore := core.LatestVersion()
	myceliumUpToDate := latestCore == "" || !core.IsNewerVersion(latestCore, core.Version)
	if !myceliumUpToDate && !body.Force {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                  false,
			"mycelium_up_to_date": false,
			"mycelium_version":    core.Version,
			"mycelium_latest":     latestCore,
			"warning":             "mycelium non è sull'ultima versione — il build web più recente potrebbe assumere funzionalità non ancora presenti in questa versione del server. Riprova con force per procedere comunque.",
		})
		return
	}

	tag, assets, err := core.LatestVersionOf(repoSlug)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"detail": fmt.Sprintf("impossibile leggere l'ultima release di %s: %v", repoSlug, err),
		})
		return
	}
	asset, ok := pickWebAsset(assets)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"detail": fmt.Sprintf("nessun asset .zip o .tar.gz che sembri il build web trovato nella release %s di %s", tag, repoSlug),
		})
		return
	}

	// Integrity: when the release ships a SHA256SUMS file the archive must
	// match it; otherwise the update proceeds and the response says so. This
	// catches a corrupted or swapped download, not a compromised release.
	expectedSHA, sumsErr := releaseChecksumFor(assets, asset.Name)
	if sumsErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"detail": "lettura checksum della release: " + sumsErr.Error()})
		return
	}

	destDir := core.AppPath("data", "pileus-web")
	if err := installPileusWebAsset(asset, destDir, expectedSHA); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
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
	writeJSON(w, http.StatusOK, resp)
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

	client := core.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
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
		aborted, _ = extractZipSafe(&zr.Reader, extractDir, maxPerFile, maxTotal, "[pileus-web]")
	}
	if aborted {
		return fmt.Errorf("archivio troppo grande una volta estratto")
	}

	if !hasIndexHTML(extractDir) {
		return fmt.Errorf("l'archivio non contiene un index.html: non sembra un build web Flutter valido")
	}

	bakDir := destDir + ".bak"
	os.RemoveAll(bakDir)
	if _, err := os.Stat(destDir); err == nil {
		if err := os.Rename(destDir, bakDir); err != nil {
			return fmt.Errorf("backup versione precedente fallito: %w", err)
		}
	}
	if err := os.Rename(extractDir, destDir); err != nil {
		// Best-effort: restore the previous version.
		if _, statErr := os.Stat(destDir); statErr != nil {
			os.Rename(bakDir, destDir)
		}
		return fmt.Errorf("installazione fallita: %w", err)
	}
	removeExtractDir = false
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

// hasIndexHTML checks for index.html at dir's root or one directory down.
func hasIndexHTML(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		return true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "index.html")); err == nil {
				return true
			}
		}
	}
	return false
}
