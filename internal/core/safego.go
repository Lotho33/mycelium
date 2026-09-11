package core

import (
	"log"
	"runtime/debug"
)

// SafeGo runs fn in its own goroutine, recovering any panic so a bug in a
// background task (a maintenance ticker, a best-effort cache write, a health
// check) logs and dies quietly instead of taking the whole process — HTTP API,
// gRPC, everything — down with it (an unrecovered panic in ANY goroutine kills
// the whole Go process, not just that goroutine). name identifies the task in
// the log line.
//
// Do NOT use this for a service's main accept/serve loop (see
// cmd/server/main.go's http.Server.Serve, internal/pileus/server.go's
// grpc.Server.Serve): if that panics, letting the process crash and restart
// (systemd/Docker) is the right behaviour — swallowing it would leave the
// process alive with no listener, a silent full outage that looks healthy.
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
