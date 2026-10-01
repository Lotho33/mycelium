package engine

import (
	"testing"
	"time"
)

// entrypointTimeout serves two call shapes:
//   - tasks (mf.Tasks): a manifest can override the budget per task via
//     timeout_seconds;
//   - entrypoints (mf.Entrypoints, e.g. "resolve" -> "resolve_stream"):
//     never in mf.Tasks, so they always get the default.

// TestEntrypointTimeout_NoTaskMatch_ReturnsDefault reproduces the exact call
// resolve_stream gets today: an entrypoint name with no corresponding entry
// in mf.Tasks at all (resolve/resolve_stream is an `entrypoints:` mapping,
// never a task). It must fall back to defaultEntrypointTimeout (90s).
func TestEntrypointTimeout_NoTaskMatch_ReturnsDefault(t *testing.T) {
	mf := LuaManifest{
		ID: "p",
		// No Tasks at all — the common case for a manifest whose entrypoints
		// are plain request/response calls (search, browse, resolve, ...)
		// with no scheduled/cron work.
	}
	got := entrypointTimeout(mf, EPResolveStream, "resolve_stream")
	if got != defaultEntrypointTimeout {
		t.Errorf("entrypointTimeout(no tasks, resolve/resolve_stream) = %v, want defaultEntrypointTimeout (%v)", got, defaultEntrypointTimeout)
	}
}

// TestEntrypointTimeout_TasksPresentButNoMatch covers the case where the
// manifest DOES declare tasks (e.g. a cron catalog refresh), but none of
// them is named "resolve" or "resolve_stream" — still must not match and
// still must fall back to the default.
func TestEntrypointTimeout_TasksPresentButNoMatch(t *testing.T) {
	mf := LuaManifest{
		ID: "p",
		Tasks: []LuaManifestTask{
			{Function: "refresh_catalog", TimeoutSec: 600},
			{Function: "enrich_noid", TimeoutSec: 120},
		},
	}
	got := entrypointTimeout(mf, EPResolveStream, "resolve_stream")
	if got != defaultEntrypointTimeout {
		t.Errorf("entrypointTimeout(unrelated tasks, resolve/resolve_stream) = %v, want defaultEntrypointTimeout (%v)", got, defaultEntrypointTimeout)
	}
}

// TestEntrypointTimeout_DefaultIs90Seconds pins the default budget.
func TestEntrypointTimeout_DefaultIs90Seconds(t *testing.T) {
	if defaultEntrypointTimeout != 90*time.Second {
		t.Errorf("defaultEntrypointTimeout = %v, want 90s", defaultEntrypointTimeout)
	}
}

// A task's own timeout_seconds wins over the default, shorter or longer.
func TestEntrypointTimeout_TaskOverrideStillWins(t *testing.T) {
	mf := LuaManifest{
		ID: "p",
		Tasks: []LuaManifestTask{
			{Function: "quick_ping", TimeoutSec: 5},
			{Function: "refresh_catalog", TimeoutSec: 600},
		},
	}
	if got, want := entrypointTimeout(mf, "quick_ping", "quick_ping"), 5*time.Second; got != want {
		t.Errorf("entrypointTimeout(quick_ping) = %v, want %v", got, want)
	}
	if got, want := entrypointTimeout(mf, "refresh_catalog", "refresh_catalog"), 600*time.Second; got != want {
		t.Errorf("entrypointTimeout(refresh_catalog) = %v, want %v", got, want)
	}
}

// TestEntrypointTimeout_TaskOverrideCappedAt15Min ensures the 15-minute cap
// on task overrides is unaffected by the default's change.
func TestEntrypointTimeout_TaskOverrideCappedAt15Min(t *testing.T) {
	mf := LuaManifest{
		ID: "p",
		Tasks: []LuaManifestTask{
			{Function: "huge_sync", TimeoutSec: 3600},
		},
	}
	got := entrypointTimeout(mf, "huge_sync", "huge_sync")
	if want := 15 * time.Minute; got != want {
		t.Errorf("entrypointTimeout(huge_sync, 3600s requested) = %v, want capped %v", got, want)
	}
}
