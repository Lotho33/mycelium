package api

import (
	"os"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// TestMain disables core.LatestVersion's background GitHub fetch for the
// whole package (tests seed the latest version themselves, and a late real
// fetch could overwrite it) and marks setup as done: admin sessions are only
// valid once an admin exists. Tests that need the setup state clear it
// themselves (withSetupNotDone).
func TestMain(m *testing.M) {
	core.DisableAutoFetchForTest()
	if !managers.Settings.IsSetupDone() {
		if err := managers.Settings.SaveInternal(map[string]any{"master_admin_hash": "test-admin-hash"}); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
