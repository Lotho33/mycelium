package core

import (
	"testing"
	"time"
)

// A panic inside SafeGo must not crash the test process (nor the real
// process) — it should be recovered, logged, and the goroutine's own defers
// still run on the way out.
func TestSafeGoRecoversPanic(t *testing.T) {
	done := make(chan struct{})
	SafeGo("test/safego-panic", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
		// fn's own defer ran despite the panic — recovery happened one frame up.
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine never completed — panic escaped SafeGo")
	}
}

// Guard, deferred directly in a func literal, stops a panic in that same
// call without needing a separate goroutine (the shape a ticker-loop
// iteration wants).
func TestGuardRecoversInPlace(t *testing.T) {
	ranAfterPanic := false
	func() {
		defer func() { ranAfterPanic = true }()
		defer Guard("test/guard-panic")
		panic("boom")
	}()
	if !ranAfterPanic {
		t.Fatal("code deferred after Guard never ran — panic wasn't recovered here")
	}
}
