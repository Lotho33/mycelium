package core

import (
	"bufio"
	"bytes"
	"compress/flate"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The machine mycelium runs on, measured once at startup: the same image
// runs on a desktop-class server and on a 4×Cortex-A35 TV box, where the
// work done in parallel (image conversions, background tasks) must be
// smaller or it competes with the client being served.
//
// Core counts don't tell the two apart (both often have 4), so the profile
// comes from a single-core micro-benchmark — deflate of a fixed 256 KiB
// buffer, best of three, ~4 ms on a Ryzen 5600G — and the memory available.
// MYCELIUM_HW_PROFILE=low|normal overrides the measurement.

// HWProfile is the class of the machine.
type HWProfile string

const (
	HWNormal HWProfile = "normal"
	HWLow    HWProfile = "low"

	// hwLowBench: slower than this on the benchmark is a low-end machine
	// (weak ARM boxes take ~15-30 ms, an Intel Atom ~15).
	hwLowBench = 10 * time.Millisecond
	// hwLowMemory: less memory than this is a low-end machine too.
	hwLowMemory = 1536 << 20
)

// Machine is what was measured.
type Machine struct {
	Profile  HWProfile
	CPUs     int
	MemBytes uint64        // 0 when unknown
	Bench    time.Duration // 0 when overridden
}

var (
	machineOnce sync.Once
	machine     Machine
)

// ThisMachine measures (once) and returns the machine's profile.
func ThisMachine() Machine {
	machineOnce.Do(func() {
		machine = measureMachine(os.Getenv("MYCELIUM_HW_PROFILE"))
		log.Printf("[core] macchina: profilo %s (%d CPU, %s di memoria, benchmark %v)",
			machine.Profile, machine.CPUs, humanMem(machine.MemBytes), machine.Bench.Round(100*time.Microsecond))
	})
	return machine
}

// LowEnd reports whether this is a low-end machine.
func LowEnd() bool { return ThisMachine().Profile == HWLow }

func measureMachine(override string) Machine {
	m := Machine{CPUs: runtime.NumCPU(), MemBytes: memTotal()}
	switch strings.ToLower(strings.TrimSpace(override)) {
	case string(HWLow):
		m.Profile = HWLow
		return m
	case string(HWNormal):
		m.Profile = HWNormal
		return m
	}
	m.Bench = cpuBench()
	m.Profile = classify(m)
	return m
}

func classify(m Machine) HWProfile {
	if m.Bench > hwLowBench || m.CPUs < 2 || (m.MemBytes > 0 && m.MemBytes < hwLowMemory) {
		return HWLow
	}
	return HWNormal
}

// cpuBench times a fixed single-core workload, best of three.
func cpuBench() time.Duration {
	buf := make([]byte, 256<<10)
	x := uint32(2463534242)
	for i := range buf {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		buf[i] = "abcdefghij klmnop"[x%17]
	}
	best := time.Duration(1<<63 - 1)
	var out bytes.Buffer
	for i := 0; i < 3; i++ {
		out.Reset()
		t := time.Now()
		w, _ := flate.NewWriter(&out, 6)
		_, _ = w.Write(buf)
		_ = w.Close()
		if d := time.Since(t); d < best {
			best = d
		}
	}
	return best
}

// The machine's total memory (/proc/meminfo), not the container's limit: the
// stack's compose files cap mycelium at 768 MB even on a large server, and
// that cap says nothing about how busy the machine is.
func memTotal() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if fields := strings.Fields(s.Text()); len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				return kb << 10
			}
		}
	}
	return 0
}

func humanMem(b uint64) string {
	if b == 0 {
		return "?"
	}
	return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GB"
}
