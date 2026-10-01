package core

import (
	"log"
	"runtime/debug"
)

// SafeGo runs fn in its own goroutine, recovering any panic so a bug in a
// background task is logged instead of killing the process (an unrecovered
// panic in any goroutine ends it). name identifies the task in the log.
//
// Not for a server's main serve loop: if that panics, crashing (and being
// restarted) beats staying up with no listener.
func SafeGo(name string, fn func()) {
	go func() {
		defer Guard(name)
		fn()
	}()
}

// Guard recovers a panic in the CURRENT goroutine, logging it under name.
// `defer core.Guard("task")` at the top of a func literal protects just that
// call without spawning a new goroutine — the shape a ticker loop wants: one
// bad tick logs and the loop keeps ticking, instead of one panic silently
// killing every future tick (SafeGo's own goroutine would exit for good).
func Guard(name string) {
	if r := recover(); r != nil {
		log.Printf("[panic] recovered in %s: %v\n%s", name, r, debug.Stack())
	}
}
