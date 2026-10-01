//go:build windows

package managers

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	psapiDLL                 = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = psapiDLL.NewProc("GetProcessMemoryInfo")
)

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func sampleProcessStats(pid int) (cpuTicks uint64, rssBytes uint64, err error) {
	// PROCESS_QUERY_LIMITED_INFORMATION = 0x1000 — sufficient on Vista+
	h, err := windows.OpenProcess(0x1000, false, uint32(pid))
	if err != nil {
		return 0, 0, fmt.Errorf("OpenProcess %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)

	var creation, exit, kernel, user windows.Filetime
	if e := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); e == nil {
		k := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
		u := uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
		cpuTicks = k + u
	}

	var mc processMemoryCounters
	mc.CB = uint32(unsafe.Sizeof(mc))
	r, _, _ := procGetProcessMemoryInfo.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&mc)),
		uintptr(mc.CB),
	)
	if r != 0 {
		rssBytes = uint64(mc.PagefileUsage)
	}
	return
}

func cpuTicksPerSecond() float64 { return 1e7 } // 100ns units → 10^7 per second
