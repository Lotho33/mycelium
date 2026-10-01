//go:build linux

package managers

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func sampleProcessStats(pid int) (cpuTicks uint64, rssBytes uint64, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[idx+2:])
	if len(fields) < 13 {
		return 0, 0, fmt.Errorf("too few fields in /proc/%d/stat", pid)
	}
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	cpuTicks = utime + stime

	statm, err2 := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err2 == nil {
		f := strings.Fields(string(statm))
		if len(f) >= 2 {
			pages, _ := strconv.ParseUint(f[1], 10, 64)
			rssBytes = pages * uint64(syscall.Getpagesize())
		}
	}
	return cpuTicks, rssBytes, nil
}

func cpuTicksPerSecond() float64 { return 100.0 }
