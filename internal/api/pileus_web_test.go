package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// setPileusWebRepo sets the pileus_web_repo setting for the duration of a
// test and restores its previous value afterwards. Goes through the same
// managers.Settings.Save the dashboard's generic settings-save endpoint
// uses, not SaveInternal — pileus_web_repo must actually be reachable
// through the "pileus_" allowlist prefix, and a test bypassing that wouldn't
// catch a regression there.
func setPileusWebRepo(t *testing.T, repo string) {
	t.Helper()
	old := managers.Settings.GetString("pileus_web_repo", "")
	if err := managers.Settings.Save(map[string]any{"pileus_web_repo": repo}); err != nil {
		t.Fatalf("save pileus_web_repo: %v", err)
	}
	t.Cleanup(func() {
		_ = managers.Settings.Save(map[string]any{"pileus_web_repo": old})
	})
}

// buildTestWebZip builds a minimal, valid-looking Flutter-web-build zip
// (just an index.html at the root) as raw bytes.
func buildTestWebZip(t *testing.T, indexContent string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	w, err := zw.Create("index.html")
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write([]byte(indexContent)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// startFakeGitHubRelease serves a fake "releases/latest" response for
// repoSlug at "/repos/<repoSlug>/releases/latest" (matching the path
// core.LatestVersionOf builds against githubAPIBase) whose single asset
// downloads zipBytes from the same server. Sets core's GitHub API base to
// this server for the duration of the test.
func startFakeGitHubRelease(t *testing.T, repoSlug, tag string, zipBytes []byte) {
	t.Helper()
	startFakeGitHubReleaseAsset(t, repoSlug, tag, "pileus-web.zip", zipBytes)
}

// startFakeGitHubReleaseAsset is startFakeGitHubRelease generalized to an
// arbitrary asset filename — used to exercise the tar.gz path (real-world
// asset name as of Lotho33/pileus v1.2.7: "pileus-1.2.7-web.tar.gz") the same
// way startFakeGitHubRelease exercises the zip path.
func startFakeGitHubReleaseAsset(t *testing.T, repoSlug, tag, assetName string, assetBytes []byte) {
	t.Helper()
	var assetURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+repoSlug+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": tag,
			"assets": []map[string]any{
				{"name": assetName, "browser_download_url": assetURL, "size": len(assetBytes)},
			},
		})
	})
	mux.HandleFunc("/download/asset", func(w http.ResponseWriter, r *http.Request) {
		w.Write(assetBytes)
	})
	srv := httptest.NewServer(mux)
	assetURL = srv.URL + "/download/asset"
	t.Cleanup(srv.Close)
	t.Cleanup(core.SetGitHubAPIBaseForTest(srv.URL))
}

// buildTestWebTarGz builds a minimal, valid-looking Flutter-web-build
// tar.gz (just an index.html at the root) as raw bytes — the tar.gz mirror
// of buildTestWebZip.
func buildTestWebTarGz(t *testing.T, indexContent string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	body := []byte(indexContent)
	if err := tw.WriteHeader(&tar.Header{
		Name: "index.html",
		Mode: 0644,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func callUpdatePileusWebApp(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/pileus-web/update", reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	updatePileusWebApp(rec, req)
	return rec
}

// TestUpdatePileusWebApp_RepoNotConfigured is the "clear message instead of
// a cryptic failure" requirement: an empty pileus_web_repo must 400 before
// any GitHub call is attempted.
func TestUpdatePileusWebApp_RepoNotConfigured(t *testing.T) {
	setPileusWebRepo(t, "")

	rec := callUpdatePileusWebApp(t, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s; want 400", rec.Code, rec.Body.String())
	}
	var d struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(d.Detail, "pileus_web_repo") {
		t.Fatalf("detail = %q, want a message pointing at pileus_web_repo", d.Detail)
	}
}

// TestUpdatePileusWebApp_CompatGateBlocksWithoutForce: mycelium not on the
// latest release must return the warning and attempt no installation when
// force isn't set.
func TestUpdatePileusWebApp_CompatGateBlocksWithoutForce(t *testing.T) {
	setPileusWebRepo(t, "Lotho33/pileus")
	withVersions(t, "1.3.2", "v1.3.3") // core.Version behind latestCore

	destDir := core.AppPath("data", "pileus-web")
	os.RemoveAll(destDir)
	t.Cleanup(func() { os.RemoveAll(destDir) })

	rec := callUpdatePileusWebApp(t, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200 (a warning, not an error)", rec.Code, rec.Body.String())
	}
	var d struct {
		OK               bool   `json:"ok"`
		MyceliumUpToDate bool   `json:"mycelium_up_to_date"`
		Warning          string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if d.OK {
		t.Fatalf("ok = true, want false — the gate should have stopped before installing anything")
	}
	if d.MyceliumUpToDate {
		t.Fatalf("mycelium_up_to_date = true, want false")
	}
	if d.Warning == "" {
		t.Fatalf("expected a non-empty warning message")
	}
	if _, err := os.Stat(destDir); err == nil {
		t.Fatalf("destDir %s was created despite the gate blocking the update", destDir)
	}
}

// TestUpdatePileusWebApp_ForceProceedsAndInstalls: with force:true the
// mismatch is only a warning — the update still runs, against a fake GitHub
// release/asset server (no real network dependency).
func TestUpdatePileusWebApp_ForceProceedsAndInstalls(t *testing.T) {
	setPileusWebRepo(t, "Lotho33/pileus")
	withVersions(t, "1.3.2", "v1.3.3")
	startFakeGitHubRelease(t, "Lotho33/pileus", "v2.0.0", buildTestWebZip(t, "<html>new build</html>"))

	destDir := core.AppPath("data", "pileus-web")
	os.RemoveAll(destDir)
	t.Cleanup(func() { os.RemoveAll(destDir); os.RemoveAll(destDir + ".bak") })

	rec := callUpdatePileusWebApp(t, map[string]any{"force": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200", rec.Code, rec.Body.String())
	}
	var d struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if !d.OK {
		t.Fatalf("ok = false, body=%s; want true (force should have let it proceed)", rec.Body.String())
	}
	if d.Version != "v2.0.0" {
		t.Fatalf("version = %q, want %q", d.Version, "v2.0.0")
	}
	data, err := os.ReadFile(destDir + "/index.html")
	if err != nil {
		t.Fatalf("index.html not installed: %v", err)
	}
	if string(data) != "<html>new build</html>" {
		t.Fatalf("index.html content = %q, want the new build's content", data)
	}
}

// TestUpdatePileusWebApp_ForceProceedsAndInstalls_TarGz mirrors
// TestUpdatePileusWebApp_ForceProceedsAndInstalls but against a tar.gz asset
// named like the real Lotho33/pileus v1.2.7 release
// ("pileus-1.2.7-web.tar.gz") — the end-to-end regression test for the bug
// that prompted this fix: pickWebAsset must recognize it and
// installPileusWebAsset must extract it with extractTarGzSafe instead of
// trying (and failing) to open it as a zip.
func TestUpdatePileusWebApp_ForceProceedsAndInstalls_TarGz(t *testing.T) {
	setPileusWebRepo(t, "Lotho33/pileus")
	withVersions(t, "1.3.2", "v1.3.3")
	startFakeGitHubReleaseAsset(t, "Lotho33/pileus", "v1.2.7", "pileus-1.2.7-web.tar.gz", buildTestWebTarGz(t, "<html>new build</html>"))

	destDir := core.AppPath("data", "pileus-web")
	os.RemoveAll(destDir)
	t.Cleanup(func() { os.RemoveAll(destDir); os.RemoveAll(destDir + ".bak") })

	rec := callUpdatePileusWebApp(t, map[string]any{"force": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200", rec.Code, rec.Body.String())
	}
	var d struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		Asset   string `json:"asset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if !d.OK {
		t.Fatalf("ok = false, body=%s; want true", rec.Body.String())
	}
	if d.Asset != "pileus-1.2.7-web.tar.gz" {
		t.Fatalf("asset = %q, want %q", d.Asset, "pileus-1.2.7-web.tar.gz")
	}
	data, err := os.ReadFile(destDir + "/index.html")
	if err != nil {
		t.Fatalf("index.html not installed: %v", err)
	}
	if string(data) != "<html>new build</html>" {
		t.Fatalf("index.html content = %q, want the new build's content", data)
	}
}

// TestUpdatePileusWebApp_BackupsPreviousVersion: a pre-existing
// data/pileus-web/ with a marker file must survive, renamed to
// data/pileus-web.bak/, after a successful update.
func TestUpdatePileusWebApp_BackupsPreviousVersion(t *testing.T) {
	setPileusWebRepo(t, "Lotho33/pileus")
	withVersions(t, "1.3.2", "1.3.2") // up to date — gate passes without force
	startFakeGitHubRelease(t, "Lotho33/pileus", "v2.0.0", buildTestWebZip(t, "<html>new build</html>"))

	destDir := core.AppPath("data", "pileus-web")
	bakDir := destDir + ".bak"
	os.RemoveAll(destDir)
	os.RemoveAll(bakDir)
	t.Cleanup(func() { os.RemoveAll(destDir); os.RemoveAll(bakDir) })

	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatalf("seed destDir: %v", err)
	}
	const marker = "OLD_BUILD_MARKER"
	if err := os.WriteFile(destDir+"/"+marker, []byte("old"), 0644); err != nil {
		t.Fatalf("seed marker file: %v", err)
	}

	rec := callUpdatePileusWebApp(t, map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(bakDir + "/" + marker); err != nil {
		t.Fatalf("marker file not found in backup dir %s: %v", bakDir, err)
	}
	if _, err := os.Stat(destDir + "/index.html"); err != nil {
		t.Fatalf("new build not installed at destDir: %v", err)
	}
	if _, err := os.Stat(destDir + "/" + marker); err == nil {
		t.Fatalf("marker file from the old build is still present in the new destDir")
	}
}

// TestPickWebAsset covers the asset-recognition criterion: name contains
// "web" (case-insensitive) and ends in ".zip", excluding anything that also
// mentions "tv" (the separate TV-flavor build).
func TestPickWebAsset(t *testing.T) {
	assets := []core.GitHubReleaseAsset{
		{Name: "Source code (zip)", BrowserDownloadURL: "https://x/src.zip"},
		{Name: "pileus-web-tv.zip", BrowserDownloadURL: "https://x/tv.zip"},
		{Name: "pileus-web.zip", BrowserDownloadURL: "https://x/web.zip"},
	}
	got, ok := pickWebAsset(assets)
	if !ok {
		t.Fatalf("pickWebAsset found nothing")
	}
	if got.Name != "pileus-web.zip" {
		t.Fatalf("picked %q, want %q", got.Name, "pileus-web.zip")
	}
}

func TestPickWebAsset_NoneMatch(t *testing.T) {
	assets := []core.GitHubReleaseAsset{
		{Name: "Source code (zip)", BrowserDownloadURL: "https://x/src.zip"},
		{Name: "checksums.txt", BrowserDownloadURL: "https://x/checksums.txt"},
	}
	if _, ok := pickWebAsset(assets); ok {
		t.Fatalf("pickWebAsset unexpectedly matched something in a release with no web asset")
	}
}

// TestPickWebAsset_TarGzOnly is the regression test for the real-world case
// found against Lotho33/pileus v1.2.7: the web build asset is published as
// "pileus-1.2.7-web.tar.gz", not a .zip. pickWebAsset must still recognize it.
func TestPickWebAsset_TarGzOnly(t *testing.T) {
	assets := []core.GitHubReleaseAsset{
		{Name: "pileus-1.2.7-android-arm64-v8a.apk", BrowserDownloadURL: "https://x/a.apk"},
		{Name: "pileus-1.2.7-web.tar.gz", BrowserDownloadURL: "https://x/web.tar.gz"},
		{Name: "SHA256SUMS.txt", BrowserDownloadURL: "https://x/sums.txt"},
	}
	got, ok := pickWebAsset(assets)
	if !ok {
		t.Fatalf("pickWebAsset found nothing, want pileus-1.2.7-web.tar.gz")
	}
	if got.Name != "pileus-1.2.7-web.tar.gz" {
		t.Fatalf("picked %q, want %q", got.Name, "pileus-1.2.7-web.tar.gz")
	}
}

// TestPickWebAsset_TgzShortSuffixAlsoMatches covers the ".tgz" spelling, not
// just ".tar.gz".
func TestPickWebAsset_TgzShortSuffixAlsoMatches(t *testing.T) {
	assets := []core.GitHubReleaseAsset{
		{Name: "pileus-web.tgz", BrowserDownloadURL: "https://x/web.tgz"},
	}
	got, ok := pickWebAsset(assets)
	if !ok {
		t.Fatalf("pickWebAsset found nothing, want pileus-web.tgz")
	}
	if got.Name != "pileus-web.tgz" {
		t.Fatalf("picked %q, want %q", got.Name, "pileus-web.tgz")
	}
}

// TestPickWebAsset_BothFormatsPresent covers a release that (unusually)
// publishes the web build in both archive formats at once: pickWebAsset must
// still return a single, deterministic choice rather than erroring out — the
// same "first match wins, log a warning" rule already used for any other
// multiple-match case. Whichever comes first in the assets slice (as
// GitHub's API returns them, generally upload order) wins; here that's the
// zip.
func TestPickWebAsset_BothFormatsPresent(t *testing.T) {
	assets := []core.GitHubReleaseAsset{
		{Name: "pileus-web.zip", BrowserDownloadURL: "https://x/web.zip"},
		{Name: "pileus-web.tar.gz", BrowserDownloadURL: "https://x/web.tar.gz"},
	}
	got, ok := pickWebAsset(assets)
	if !ok {
		t.Fatalf("pickWebAsset found nothing")
	}
	if got.Name != "pileus-web.zip" {
		t.Fatalf("picked %q, want %q (first match wins)", got.Name, "pileus-web.zip")
	}
}
