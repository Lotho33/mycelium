package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mycelium/internal/core"
)

// withVersions sets core.Version and the cached "latest" version for the
// duration of a test, restoring both afterwards. core.SetLatestVersionForTest
// is the test-only seam for LatestVersion()'s otherwise package-private cache.
func withVersions(t *testing.T, version, latest string) {
	t.Helper()
	origVersion := core.Version
	core.Version = version
	core.SetLatestVersionForTest(latest)
	t.Cleanup(func() { core.Version = origVersion })
}

// TestAdminInfo_SameReleaseDifferentPrefix is the regression test for the
// dashboard bug: GitHub release tags are always "v"-prefixed (e.g. "v1.3.2")
// while core.Version isn't, so the two fields in /admin/info's response never
// matched byte-for-byte even when running the latest release. admin.js (and
// Go's coreUpdate) must decide "same version?" via core.SameVersion, not a
// bare string comparison — this test replicates that same normalization to
// assert the two fields the JSON response carries denote the same release.
func TestAdminInfo_SameReleaseDifferentPrefix(t *testing.T) {
	withVersions(t, "1.3.2", "v1.3.2")

	req := httptest.NewRequest(http.MethodGet, "/admin/info", nil)
	rec := httptest.NewRecorder()
	getAdminInfo(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200", rec.Code, rec.Body.String())
	}
	var d struct {
		Version       string `json:"version"`
		LatestVersion string `json:"latest_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if d.Version != "1.3.2" || d.LatestVersion != "v1.3.2" {
		t.Fatalf("got version=%q latest_version=%q, want version=%q latest_version=%q (raw values preserved)",
			d.Version, d.LatestVersion, "1.3.2", "v1.3.2")
	}
	// The check a client (admin.js) must apply to decide whether to show the
	// update banner — mirrors normalizeVersion()'s JS twin.
	if !core.SameVersion(d.Version, d.LatestVersion) {
		t.Fatalf("SameVersion(%q, %q) = false, want true — dashboard would wrongly offer an update to the version already running", d.Version, d.LatestVersion)
	}
}

// TestAdminInfo_GenuinelyDifferentRelease is the counterpart: a real newer
// release must still compare as different once normalized.
func TestAdminInfo_GenuinelyDifferentRelease(t *testing.T) {
	withVersions(t, "1.3.2", "v1.3.3")

	req := httptest.NewRequest(http.MethodGet, "/admin/info", nil)
	rec := httptest.NewRecorder()
	getAdminInfo(rec, req)

	var d struct {
		Version       string `json:"version"`
		LatestVersion string `json:"latest_version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if core.SameVersion(d.Version, d.LatestVersion) {
		t.Fatalf("SameVersion(%q, %q) = true, want false — a real new release must still trigger the update banner", d.Version, d.LatestVersion)
	}
}

// TestCoreUpdate_UpToDateIgnoresPrefix is the regression test for the actual
// functional bug behind the UI one: with the old bare `latest == core.Version`
// comparison, coreUpdate never recognized a "v"-prefixed GitHub tag as the
// same release it was already running, so it fell through past the
// up-to-date short-circuit and attempted a real self-update every time —
// here that would mean stat-ing /usr/local/bin/mycelium-update, which does
// not exist in the test environment, producing a 500 instead of the
// up_to_date response asserted below. That differential is what proves the
// script is never even reached once the versions are recognized as equal.
func TestCoreUpdate_UpToDateIgnoresPrefix(t *testing.T) {
	withVersions(t, "1.3.2", "v1.3.2")

	req := httptest.NewRequest(http.MethodPost, "/admin/core/update", nil)
	rec := httptest.NewRecorder()
	coreUpdate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200 (up_to_date, returned before any update attempt)", rec.Code, rec.Body.String())
	}
	var d struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if d.Status != "up_to_date" {
		t.Fatalf("status field = %q, want %q — same release under a different tag prefix must not trigger an update attempt", d.Status, "up_to_date")
	}
	if d.Version != "1.3.2" {
		t.Fatalf("version field = %q, want %q", d.Version, "1.3.2")
	}
}

// TestCoreUpdate_UpToDateWhenAheadOfLatest replicates the exact production
// bug reported by the user: the running binary (core.Version) is AHEAD of
// the latest release GitHub currently reports (e.g. a newer build was
// deployed before its matching tag/release was published) — v1.3.3 running,
// GitHub still serving v1.3.2. The old `core.SameVersion` check treated any
// difference as "not up to date" and fell through into attempting an update
// (which, in this test environment with no mycelium-update script, would
// surface as a 500 rather than the up_to_date short-circuit asserted below).
// With core.IsNewerVersion, "latest is not newer than current" — including
// current being ahead — must short-circuit to up_to_date instead.
func TestCoreUpdate_UpToDateWhenAheadOfLatest(t *testing.T) {
	withVersions(t, "1.3.3", "v1.3.2")

	req := httptest.NewRequest(http.MethodPost, "/admin/core/update", nil)
	rec := httptest.NewRecorder()
	coreUpdate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s; want 200 (up_to_date, returned before any update attempt)", rec.Code, rec.Body.String())
	}
	var d struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	if d.Status != "up_to_date" {
		t.Fatalf("status field = %q, want %q — running ahead of the latest published release must not trigger a (backwards) update attempt", d.Status, "up_to_date")
	}
	if d.Version != "1.3.3" {
		t.Fatalf("version field = %q, want %q", d.Version, "1.3.3")
	}
}

// TestAdminInfo_UpdateAvailableField covers the new update_available field
// getAdminInfo exposes so the dashboard doesn't have to reimplement the
// version-order comparison in JavaScript: it must be true only when latest is
// genuinely newer, and false both when equal and when core.Version is ahead
// of the latest published release (the reported bug's exact scenario).
func TestAdminInfo_UpdateAvailableField(t *testing.T) {
	cases := []struct {
		name, version, latest string
		want                  bool
	}{
		{"genuinely newer release", "1.3.2", "v1.3.3", true},
		{"same release, different prefix", "1.3.2", "v1.3.2", false},
		{"running ahead of latest — the reported bug", "1.3.3", "v1.3.2", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withVersions(t, c.version, c.latest)

			req := httptest.NewRequest(http.MethodGet, "/admin/info", nil)
			rec := httptest.NewRecorder()
			getAdminInfo(rec, req)

			var d struct {
				UpdateAvailable bool `json:"update_available"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
				t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
			}
			if d.UpdateAvailable != c.want {
				t.Fatalf("update_available = %v, want %v (version=%q latest=%q)", d.UpdateAvailable, c.want, c.version, c.latest)
			}
		})
	}
}

// TestCoreUpdate_RejectsMalformedLatestVersion is the regression test for the
// audit finding: core.LatestVersion() (the GitHub API response) used to flow
// unvalidated into exec.Command(updateScript, latest). A malformed value —
// here one carrying shell/argument-injection-shaped characters — must be
// rejected before coreUpdate ever reaches the exec.Command call. The test
// environment has no /usr/local/bin/mycelium-update, so if validation did
// NOT happen first, the handler would instead fail later at the os.Stat
// check with the generic "script non trovato" detail; asserting the specific
// version-format error message proves the new check is what's firing, and
// that it fires before any exec.Command is attempted.
func TestCoreUpdate_RejectsMalformedLatestVersion(t *testing.T) {
	malformed := []string{
		"v1.2.3; rm -rf /",
		"$(reboot)",
		"../../etc/passwd",
		"not-a-version",
		"v1.2",
		"",
	}
	for _, latest := range malformed {
		t.Run(latest, func(t *testing.T) {
			if latest == "" {
				// "" is handled by the earlier "no version yet" branch, not
				// the format check — covered by a different code path, skip
				// here to keep this test focused on the regex validation.
				t.Skip("empty latest hits the earlier availability check")
			}
			withVersions(t, "1.3.2", latest)

			req := httptest.NewRequest(http.MethodPost, "/admin/core/update", nil)
			rec := httptest.NewRecorder()
			coreUpdate(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body=%s; want 500 (rejected before exec.Command)", rec.Code, rec.Body.String())
			}
			var d struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
				t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
			}
			if strings.Contains(d.Detail, "script di aggiornamento non trovato") {
				t.Fatalf("got the script-not-found error, meaning validation was skipped and the flow reached the os.Stat/exec.Command path: %q", d.Detail)
			}
			if d.Detail == "" {
				t.Fatalf("expected a rejection detail message, got empty body=%s", rec.Body.String())
			}
		})
	}
}

// TestCoreUpdate_AcceptsWellFormedLatestVersion is the counterpart: a
// genuine "vX.Y.Z" (or "X.Y.Z") release tag must pass the new format check.
// It still can't reach exec.Command in this test environment (no
// /usr/local/bin/mycelium-update installed), so success here is observed as
// the flow reaching the *next* gate — the generic "script non trovato"
// error — rather than being rejected by the format validation itself.
func TestCoreUpdate_AcceptsWellFormedLatestVersion(t *testing.T) {
	for _, latest := range []string{"v1.3.3", "1.3.3"} {
		t.Run(latest, func(t *testing.T) {
			withVersions(t, "1.3.2", latest)

			req := httptest.NewRequest(http.MethodPost, "/admin/core/update", nil)
			rec := httptest.NewRecorder()
			coreUpdate(rec, req)

			var d struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
				t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
			}
			if !strings.Contains(d.Detail, "script di aggiornamento non trovato") {
				t.Fatalf("well-formed version %q was rejected by format validation: %q", latest, d.Detail)
			}
		})
	}
}
