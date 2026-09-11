package managers

import (
	"math"
	"os"
	"sync"
	"time"
)

// ResourceStats holds a sampled CPU/RSS reading for a process.
type ResourceStats struct {
	PID        int     `json:"pid"`
	MemBytes   int64   `json:"mem_bytes"`
	CPUPercent float64 `json:"cpu_percent"`
	CPUMs      int64   `json:"cpu_ms"`
	UptimeSec  int64   `json:"uptime_sec"`
}

// liveSampler tracks the previous CPU-ticks reading for a single logical
// resource (a PID that may change across restarts — the core process itself,
// or tailscaled) so each call to sample can compute a CPU% delta against the
// last call.
type liveSampler struct {
	mu       sync.Mutex
	prevTick uint64
	prevAt   time.Time
	startAt  time.Time
}

func newLiveSampler() *liveSampler {
	return &liveSampler{startAt: time.Now()}
}

// sample reads pid's current CPU/RSS and returns a ResourceStats with CPU%
// computed against this sampler's previous reading. ok is false if pid is 0
// or the /proc read fails (process not found / not running).
func (s *liveSampler) sample(pid int) (stats ResourceStats, ok bool) {
	if pid == 0 {
		return ResourceStats{}, false
	}
	ticks, rss, err := sampleProcessStats(pid)
	if err != nil {
		return ResourceStats{PID: pid}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	cpuPct := 0.0
	cpuMs := int64(0)
	if !s.prevAt.IsZero() {
		elapsed := now.Sub(s.prevAt).Seconds()
		tps := cpuTicksPerSecond()
		if elapsed > 0 && ticks >= s.prevTick {
			delta := float64(ticks-s.prevTick) / tps
			cpuPct = math.Round((delta/elapsed)*100*10) / 10
			cpuMs = int64(delta * 1000)
		}
	}
	s.prevTick = ticks
	s.prevAt = now

	return ResourceStats{
		PID:        pid,
		MemBytes:   int64(rss),
		CPUPercent: cpuPct,
		CPUMs:      cpuMs,
		UptimeSec:  int64(now.Sub(s.startAt).Seconds()),
	}, true
}

var coreSampler = newLiveSampler()

// GetCoreStats samples CPU and RSS of the current process.
func GetCoreStats() ResourceStats {
	stats, _ := coreSampler.sample(os.Getpid())
	return stats
}
