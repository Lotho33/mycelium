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

// Release tags are "v"-prefixed while core.Version may not be: the two
// /admin/info fields must still denote the same release once normalized.
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

// A genuinely newer release compares as different.
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

// coreUpdate recognizes a "v"-prefixed tag as the running release and
// answers up_to_date.
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

// Running ahead of the latest published release (v1.3.3 vs v1.3.2) is up
// to date, not an "update" backwards.
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

// update_available is true only when latest is genuinely newer.
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

// A malformed latest version (injection-shaped characters) is rejected with
// the version-format error before anything else happens.
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
			if strings.Contains(d.Detail, "solo in Docker") {
				t.Fatalf("got the manual_required answer, meaning validation was skipped: %q", d.Detail)
			}
			if d.Detail == "" {
				t.Fatalf("expected a rejection detail message, got empty body=%s", rec.Body.String())
			}
		})
	}
}

// TestCoreUpdate_AcceptsWellFormedLatestVersion is the counterpart: a
// genuine "vX.Y.Z" (or "X.Y.Z") release tag must pass the new format check.
// Success is observed as the flow reaching the next step — the
// "manual_required" answer (Docker is the only supported deployment) —
// rather than being rejected by the format validation itself.
func TestCoreUpdate_AcceptsWellFormedLatestVersion(t *testing.T) {
	for _, latest := range []string{"v1.3.3", "1.3.3"} {
		t.Run(latest, func(t *testing.T) {
			withVersions(t, "1.3.2", latest)

			req := httptest.NewRequest(http.MethodPost, "/admin/core/update", nil)
			rec := httptest.NewRecorder()
			coreUpdate(rec, req)

			var d struct {
				Status string `json:"status"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
				t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
			}
			if d.Status != "manual_required" {
				t.Fatalf("well-formed version %q was rejected by format validation: %q", latest, d.Detail)
			}
		})
	}
}
