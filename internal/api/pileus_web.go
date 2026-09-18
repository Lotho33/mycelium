// Pileus web-app updater: POST /admin/pileus-web/update fetches the latest
// release of a configurable Pileus repo (setting "pileus_web_repo", format
// "owner/repo" — the exact repo isn't fixed yet, so it must never be
// hardcoded, see managers/settings.go), downloads its Flutter web build
// asset, and installs it into data/pileus-web/ — the directory serveWebApp
// (setup.go) already serves at /app. Same admin-session auth boundary as
// every other /admin/* route.
package api

import (
	"archive/zip"
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

// defaultPileusWebRepo seeds the "pileus_web_repo" setting before an admin
// ever saves one explicitly — the official repo, confirmed reachable
// (github.com/Lotho33/pileus, public releases). Still just a default: the
// setting stays fully overridable from the dashboard, never hardcoded into
// the request path itself. Shared with getAdminInfo (admin_system.go) so the
// dashboard's precompiled field and this handler's actual behaviour never
// disagree about what "not yet configured" falls back to.
const defaultPileusWebRepo = "Lotho33/pileus"

// repoSlugRe validates the "owner/repo" shape of the pileus_web_repo setting
// before it's concatenated into a GitHub API URL (core.LatestVersionOf).
// It's an admin-only setting (not attacker-reachable input), but constraining
// it to the expected shape is cheap and avoids surprising requests to
// api.github.com built from a fat-fingered value.
var repoSlugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// maxPileusWebAssetBytes caps the downloaded release asset, whichever archive
// format it arrives in (.zip or .tar.gz/.tgz — see pickWebAsset). A compiled
// Flutter web build is typically 10-30 MiB; 100 MiB leaves generous headroom
// for growth (more locales, larger assets bundled in) while still refusing a
// wildly oversized or misconfigured asset outright.
const maxPileusWebAssetBytes = 100 << 20

// RegisterPileusWebRoutes wires the Pileus web-app updater endpoint onto mux.
func RegisterPileusWebRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/pileus-web/update", adminAuthMiddleware(updatePileusWebApp))
}

// updatePileusWebApp godoc
//
//	@Summary		Aggiorna l'app web Pileus
//	@Description	Scarica l'ultima release del repo Pileus configurato (pileus_web_repo) e la installa in data/pileus-web/, servita da /app. Se mycelium stesso non è sull'ultima versione, risponde con un avviso e non installa nulla a meno di force:true.
//	@Tags			Admin
//	@Accept			json
//	@Param			body	body	object{force=bool}	false	"force:true per procedere nonostante l'avviso di versione"
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/pileus-web/update [post]
func updatePileusWebApp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Force bool `json:"force"`
	}
	// Body opzionale: un POST senza body equivale a force:false, non a un
	// errore — così un semplice "aggiorna" da dashboard senza dover costruire
	// un JSON minimale funziona lo stesso finché il gate di versione passa.
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

	// Gate di compatibilità: avvisa (senza bloccare) se mycelium stesso non è
	// sull'ultima release — un core datato potrebbe non avere ancora una
	// funzionalità che il nuovo build web dà per scontata. core.LatestVersion()
	// è la cache oraria fire-and-forget di questo stesso repo (mycelium-core);
	// "" significa "non ancora recuperata", trattato come "sconosciuto" — né
	// bloccante né dichiarato up-to-date.
	latestCore := core.LatestVersion()
	// !IsNewerVersion copre sia "stessa versione" sia "mycelium è già avanti
	// rispetto all'ultima release pubblicata" — SameVersion (uguaglianza di
	// stringa) trattava quest'ultimo caso come "non aggiornato" e bloccava
	// l'update dell'app web dietro il gate di compatibilità senza motivo.
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

	destDir := core.AppPath("data", "pileus-web")
	if err := installPileusWebAsset(asset, destDir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}

	log.Printf("[pileus-web] app web aggiornata a %s (asset %s) da %s", tag, asset.Name, repoSlug)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"version":             tag,
		"asset":               asset.Name,
		"mycelium_up_to_date": myceliumUpToDate,
	})
}

// pickWebAsset chooses the release asset that looks like the Flutter web
// build among a release's assets. Criterion: the (lowercased) name contains
// "web" and ends in ".zip", ".tar.gz" or ".tgz" (isSupportedWebArchiveName) —
// permissive enough to match "pileus-web.zip", "pileus-1.2.7-web.tar.gz",
// "web-build.tgz", etc. without also matching GitHub's automatic "Source code
// (zip)" entry (that one is named after the repo/tag, not "web"). Accepting
// either archive format is deliberate: Pileus's web build shipped as .zip
// historically and switched to .tar.gz as of v1.2.7 — nothing here should
// assume either is permanent. An asset whose name also contains "tv" is
// skipped here — it's assumed to be the separate TV-flavor build
// (data/pileus-web-tv/, served at /app-tv), not installed by this endpoint.
//
// If more than one asset still matches (including a same-release mix of both
// archive formats — unlikely, but not ruled out), the first one wins and a
// warning is logged rather than failing the request outright: a deterministic
// single choice beats sinking the whole update over an ambiguously-published
// release, and the log line is there for whoever cuts the release to notice
// and fix the asset naming for next time. installPileusWebAsset then decides
// which extractor to use from that same chosen asset's name.
func pickWebAsset(assets []core.GitHubReleaseAsset) (core.GitHubReleaseAsset, bool) {
	var match core.GitHubReleaseAsset
	found := false
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if !isSupportedWebArchiveName(name) || !strings.Contains(name, "web") {
			continue
		}
		if strings.Contains(name, "tv") {
			continue // separate TV-flavor asset, not this endpoint's job
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

// isSupportedWebArchiveName reports whether lowerName (already lowercased)
// ends in an archive extension this endpoint knows how to extract. Shared by
// pickWebAsset (recognition) and installPileusWebAsset (which extractor to
// call), so the two never drift apart on what "a web archive" means.
func isSupportedWebArchiveName(lowerName string) bool {
	return strings.HasSuffix(lowerName, ".zip") ||
		strings.HasSuffix(lowerName, ".tar.gz") ||
		strings.HasSuffix(lowerName, ".tgz")
}

// isTarGzArchiveName reports whether lowerName looks like a gzipped tar
// (".tar.gz" or ".tgz") as opposed to a zip — the only distinction
// installPileusWebAsset needs once isSupportedWebArchiveName has already
// confirmed the name is one of the two.
func isTarGzArchiveName(lowerName string) bool {
	return strings.HasSuffix(lowerName, ".tar.gz") || strings.HasSuffix(lowerName, ".tgz")
}

// installPileusWebAsset downloads asset's archive (.zip or .tar.gz/.tgz —
// whichever isSupportedWebArchiveName matched it as), extracts it into a
// fresh temp directory next to destDir with the matching safe extractor
// (extractZipSafe or extractTarGzSafe — same zip-slip/tar-slip/size
// protections either way, see zip_extract.go and tar_extract.go), checks it
// looks like a Flutter web build (an index.html at the root, or one level
// down — both extractors already strip a single common wrapper folder, but
// Flutter output is normally flat so this is mostly a sanity check), and
// swaps it in for destDir:
//
//  1. any stale destDir+".bak" from a previous update is removed — this is a
//     single backup slot, not a history;
//  2. the current destDir (if present) is renamed to destDir+".bak" — the
//     rollback path, restored by hand if the new build misbehaves;
//  3. the validated extraction is renamed into destDir.
//
// Both renames are as close to atomic as os.Rename gives on the same
// filesystem (extractDir is created as a sibling of destDir specifically so
// the final rename is same-filesystem, not a cross-device copy).
func installPileusWebAsset(asset core.GitHubReleaseAsset, destDir string) error {
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

	// isSupportedWebArchiveName already confirmed asset.Name matches one of
	// the two formats this endpoint understands — isTarGzArchiveName just
	// picks which one, so the temp file extension (harmless either way) and
	// the extractor choice below stay in sync with what pickWebAsset saw.
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

	// Read one byte past the cap so an oversized upstream is detected instead
	// of silently truncated into a corrupt (but under-cap-sized) archive.
	n, err := io.Copy(tmpArchive, io.LimitReader(resp.Body, maxPileusWebAssetBytes+1))
	tmpArchive.Close()
	if err != nil {
		return fmt.Errorf("scrittura archivio temporaneo: %w", err)
	}
	if n > maxPileusWebAssetBytes {
		return fmt.Errorf("asset troppo grande (oltre %d MiB)", maxPileusWebAssetBytes>>20)
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

	// Caps sized for a Flutter web build: main.dart.js is usually the largest
	// single file (a few MiB, occasionally more with heavy asset bundling);
	// the whole tree stays well under the 150 MiB total even with several
	// locale/font variants.
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
		// Best-effort: put the previous version back rather than leaving
		// destDir missing entirely.
		if _, statErr := os.Stat(destDir); statErr != nil {
			os.Rename(bakDir, destDir)
		}
		return fmt.Errorf("installazione fallita: %w", err)
	}
	removeExtractDir = false
	return nil
}

// hasIndexHTML checks for index.html at dir's root or exactly one
// subdirectory down — covers a plain `flutter build web` output (index.html
// at the root; the common case, and the one extractZipSafe's single-wrapper
// stripping already normalizes to) as well as an archive whose entries didn't
// uniformly share one top folder (so the stripping didn't kick in) but still
// nests the build one level deep.
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
