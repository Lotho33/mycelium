package core

import (
	"testing"
	"time"
)

func TestClassifyMachine(t *testing.T) {
	gb := uint64(1 << 30)
	cases := []struct {
		m    Machine
		want HWProfile
	}{
		{Machine{CPUs: 2, MemBytes: 4 * gb, Bench: 4 * time.Millisecond}, HWNormal}, // the home server
		{Machine{CPUs: 4, MemBytes: 2 * gb, Bench: 25 * time.Millisecond}, HWLow},   // a Cortex-A35 TV box
		{Machine{CPUs: 4, MemBytes: gb, Bench: 5 * time.Millisecond}, HWLow},        // little memory
		{Machine{CPUs: 1, MemBytes: 8 * gb, Bench: 4 * time.Millisecond}, HWLow},    // one core
		{Machine{CPUs: 8, MemBytes: 0, Bench: 3 * time.Millisecond}, HWNormal},      // memory unknown
	}
	for i, c := range cases {
		if got := classify(c.m); got != c.want {
			t.Errorf("case %d: %+v → %s, want %s", i, c.m, got, c.want)
		}
	}
}

func TestMeasureMachine_Override(t *testing.T) {
	if m := measureMachine("LOW"); m.Profile != HWLow || m.Bench != 0 {
		t.Fatalf("override low: %+v", m)
	}
	if m := measureMachine("normal"); m.Profile != HWNormal {
		t.Fatalf("override normal: %+v", m)
	}
	if m := measureMachine(""); m.Bench <= 0 || m.CPUs <= 0 {
		t.Fatalf("measured: %+v", m)
	}
}
