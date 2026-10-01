package api

import (
	"os"
	"testing"

	"mycelium/internal/core"
)

// TestMain disables core.LatestVersion's background GitHub fetch for the
// whole package: tests seed the latest version themselves, and a late real
// fetch could overwrite it.
func TestMain(m *testing.M) {
	core.DisableAutoFetchForTest()
	os.Exit(m.Run())
}
