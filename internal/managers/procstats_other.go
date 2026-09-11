//go:build !linux && !windows

package managers

import "fmt"

func sampleProcessStats(pid int) (cpuTicks uint64, rssBytes uint64, err error) {
	return 0, 0, fmt.Errorf("resource monitoring not supported on this platform")
}

func cpuTicksPerSecond() float64 { return 1.0 }
