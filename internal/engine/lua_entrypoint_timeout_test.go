package engine

import (
	"testing"
	"time"
)

// entrypointTimeout is used for two very different call shapes:
//   - Tasks (mf.Tasks): a manifest can override the budget per-task via
//     timeout_seconds.
//   - Entrypoints (mf.Entrypoints, e.g. "resolve" -> "resolve_stream"): these
//     are NEVER present in mf.Tasks, so the loop in entrypointTimeout can
//     never match them and they always fall through to the default.
//
// The tests below pin that behavior down, in particular the exact scenario
// that caused resolve_stream in the bundled scraper plugins (whose several
// sequential network calls can together take up to ~90s "hot") to be timed
// out far too early under the old 30s default (see defaultEntrypointTimeout
// doc comment in lua_plugin.go).

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

// TestEntrypointTimeout_DefaultIs90Seconds pins the exact constant value:
// 90s comfortably covers the "hot" (domain already cached) worst-case of
// ~90s measured across vix.movie, vix.series and animeunity's resolve_stream
// chains. The old 30s value is what made a moderately slow (not broken)
// resolve time out virtually every time.
func TestEntrypointTimeout_DefaultIs90Seconds(t *testing.T) {
	if defaultEntrypointTimeout != 90*time.Second {
		t.Errorf("defaultEntrypointTimeout = %v, want 90s", defaultEntrypointTimeout)
	}
}

// TestEntrypointTimeout_TaskOverrideStillWins ensures the fix didn't touch
// the (already correct) task-override path: a task's own timeout_seconds
// must still win over the default, in both directions (shorter and longer
// than the new 90s default).
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
			{Function: "huge_scrape", TimeoutSec: 3600},
		},
	}
	got := entrypointTimeout(mf, "huge_scrape", "huge_scrape")
	if want := 15 * time.Minute; got != want {
		t.Errorf("entrypointTimeout(huge_scrape, 3600s requested) = %v, want capped %v", got, want)
	}
}
