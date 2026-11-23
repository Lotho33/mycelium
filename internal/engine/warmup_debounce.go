package engine

import (
	"sync"
	"time"
)

// Catalog counts (warmupCatalogCounts) run after every task, and a live
// plugin finishes a task every minute or two: each run calls get_catalog for
// every auto_hide_when_empty catalog, ~1000 calls an hour on a server with a
// couple of live plugins and nobody watching. The counts only decide whether
// an empty carousel is hidden (cached for 30 minutes), so they are refreshed
// at most once per warmupMinGap per plugin; tasks finishing in between are
// folded into one trailing run, so the counts never lag by more than the gap.
const warmupMinGap = 5 * time.Minute

type warmupState struct {
	last    time.Time // start of the last run
	pending bool      // a trailing run is scheduled
}

var (
	warmupMu     sync.Mutex
	warmupStates = map[string]*warmupState{}
	// warmupNow and warmupAfter are seams for tests.
	warmupNow   = time.Now
	warmupAfter = time.AfterFunc
)

// scheduleWarmup runs fn (the plugin's catalog counts) now if the last run
// is at least warmupMinGap old, otherwise once when the gap is over.
func scheduleWarmup(pluginID string, fn func()) {
	warmupMu.Lock()
	defer warmupMu.Unlock()
	st := warmupStates[pluginID]
	if st == nil {
		st = &warmupState{}
		warmupStates[pluginID] = st
	}
	if st.pending {
		return
	}
	now := warmupNow()
	wait := warmupMinGap - now.Sub(st.last)
	if st.last.IsZero() || wait <= 0 {
		st.last = now
		go fn()
		return
	}
	st.pending = true
	warmupAfter(wait, func() {
		warmupMu.Lock()
		st.pending = false
		st.last = warmupNow()
		warmupMu.Unlock()
		fn()
	})
}

// forgetWarmup drops pluginID's state (plugin unloaded or reinstalled: the
// next load counts at once).
func forgetWarmup(pluginID string) {
	warmupMu.Lock()
	delete(warmupStates, pluginID)
	warmupMu.Unlock()
}
