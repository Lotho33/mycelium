package engine

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduleWarmup_AtMostOncePerGapWithTrailingRun(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	var timers []func()
	warmupNow = func() time.Time { return now }
	warmupAfter = func(d time.Duration, f func()) *time.Timer {
		if d <= 0 || d > warmupMinGap {
			t.Errorf("trailing run scheduled in %v", d)
		}
		timers = append(timers, f)
		return nil
	}
	defer func() { warmupNow, warmupAfter = time.Now, time.AfterFunc }()
	defer forgetWarmup("p")

	var runs atomic.Int32
	done := make(chan struct{}, 8)
	fn := func() { runs.Add(1); done <- struct{}{} }

	scheduleWarmup("p", fn) // first: at once
	<-done
	now = now.Add(time.Minute)
	for i := 0; i < 5; i++ { // five tasks within the gap: one trailing run
		scheduleWarmup("p", fn)
	}
	if len(timers) != 1 {
		t.Fatalf("%d trailing runs scheduled, want 1", len(timers))
	}
	if runs.Load() != 1 {
		t.Fatalf("ran %d times before the gap was over", runs.Load())
	}
	now = now.Add(warmupMinGap)
	timers[0]()
	<-done
	if runs.Load() != 2 {
		t.Fatalf("trailing run missing: %d runs", runs.Load())
	}
	// Right after the trailing run, a new task waits for the gap again.
	scheduleWarmup("p", fn)
	if len(timers) != 2 || runs.Load() != 2 {
		t.Fatalf("timers %d runs %d", len(timers), runs.Load())
	}
	// Another plugin is independent.
	scheduleWarmup("q", fn)
	<-done
	forgetWarmup("q")
	if runs.Load() != 3 {
		t.Fatalf("other plugin delayed: %d runs", runs.Load())
	}
}
