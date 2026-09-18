package api

import (
	"os"
	"testing"

	"mycelium/internal/core"
)

// TestMain runs once for the whole package's test binary, before any test
// function. core.DisableAutoFetchForTest stops core.LatestVersion() from ever
// spawning its real background fetch to api.github.com during these tests —
// several tests here call handlers (getAdminInfo, coreUpdate,
// updatePileusWebApp) that read core.LatestVersion() after seeding it via
// core.SetLatestVersionForTest for their own scenario; without this, whichever
// test happens to be the first ever caller in the binary kicks off a real
// network fetch whose late completion can silently overwrite another test's
// seeded value mid-run — an observed flake, not a hypothetical one.
func TestMain(m *testing.M) {
	core.DisableAutoFetchForTest()
	os.Exit(m.Run())
}
