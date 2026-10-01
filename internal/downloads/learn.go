package downloads

import (
	"fmt"
	"time"

	"mycelium/internal/managers"
)

// ─── learned quality by hour ─────────────────────────────────────────────────
// Every probe (download options, jobs) records the best height a plugin
// offered at that hour of the day. From that the server knows the usual
// maximum ("ora 720p, di solito 1080p") and, when a plugin doesn't declare
// preferred_hours, can suggest the hours when it serves its best quality —
// no plugin work needed. Observations older than learnWindow are replaced, so
// a source that changes its behaviour is re-learned.

const (
	learnWindow   = 21 * 24 * time.Hour
	learnMinHours = 12 // hours of the day that must be covered before suggesting a window
)

// recordQuality notes height as seen for pluginID at t.
func recordQuality(pluginID string, height int, t time.Time) {
	if height <= 0 || pluginID == "" {
		return
	}
	hour := t.In(time.Local).Hour()
	cutoff := t.Add(-learnWindow).Unix()
	_, _ = managers.DB.Exec(`INSERT INTO dl_quality_hourly(plugin_id, hour, max_height, updated_at) VALUES(?,?,?,?)
		ON CONFLICT(plugin_id, hour) DO UPDATE SET
			max_height = CASE WHEN updated_at < ? THEN excluded.max_height ELSE MAX(max_height, excluded.max_height) END,
			updated_at = excluded.updated_at`,
		pluginID, hour, height, t.Unix(), cutoff)
}

// hourlyQuality returns the recent best height per hour (-1 = no data).
func hourlyQuality(pluginID string, now time.Time) [24]int {
	var out [24]int
	for i := range out {
		out[i] = -1
	}
	rows, err := managers.DB.Query(`SELECT hour, max_height FROM dl_quality_hourly WHERE plugin_id=? AND updated_at>=?`,
		pluginID, now.Add(-learnWindow).Unix())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var h, v int
		if rows.Scan(&h, &v) == nil && h >= 0 && h < 24 {
			out[h] = v
		}
	}
	return out
}

// usualMaxHeight is the best height pluginID served recently at any hour.
func usualMaxHeight(pluginID string, now time.Time) int {
	best := 0
	for _, v := range hourlyQuality(pluginID, now) {
		best = max(best, v)
	}
	return best
}

// learnedWindow suggests "HH:00-HH:00": the longest run of consecutive hours
// (wrapping midnight) in which pluginID reached its best quality. "" when
// there isn't enough data, or the quality doesn't depend on the hour.
func learnedWindow(pluginID string, now time.Time) string {
	q := hourlyQuality(pluginID, now)
	covered, best := 0, 0
	for _, v := range q {
		if v >= 0 {
			covered++
			best = max(best, v)
		}
	}
	if covered < learnMinHours || best == 0 {
		return ""
	}
	lower := false
	for _, v := range q {
		if v >= 0 && v < best {
			lower = true
		}
	}
	if !lower {
		return "" // same quality whenever observed: no reason to wait
	}
	// Longest circular run of hours at the best quality.
	bestStart, bestLen := -1, 0
	for start := 0; start < 24; start++ {
		if q[start] != best || q[(start+23)%24] == best {
			continue // not the beginning of a run
		}
		n := 0
		for n < 24 && q[(start+n)%24] == best {
			n++
		}
		if n > bestLen {
			bestStart, bestLen = start, n
		}
	}
	if bestStart < 0 || bestLen >= 24 {
		return ""
	}
	return fmt.Sprintf("%02d:00-%02d:00", bestStart, (bestStart+bestLen)%24)
}

// preferredWindow is the download window for pluginID: the manifest's
// preferred_hours, else the learned one. source is "plugin", "learned" or "".
func preferredWindow(pluginID string, now time.Time) (hours, source string) {
	if h := pluginDownloadConfig(pluginID).PreferredHours; h != "" {
		if _, ok := parseWindow(h); ok {
			return h, "plugin"
		}
	}
	if h := learnedWindow(pluginID, now); h != "" {
		return h, "learned"
	}
	return "", ""
}
