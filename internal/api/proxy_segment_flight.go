package api

import (
	"sync"
	"sync/atomic"
	"time"
)

// Shared segment fetches. A player that times out on a slow segment (ExoPlayer
// gives up after ~8s) asks for the same URL again; without sharing, the first
// fetch was cancelled with the player's request and the retry started from
// zero. Now the first request for a URL leads an upstream fetch that is not
// tied to its player connection: a retry arriving meanwhile waits for it, and
// if the original player is gone (or someone waited) the normalised bytes go
// to the warm cache (managers.Segments), where the retry finds them.

// segmentAttemptTTFB bounds how long one upstream attempt may wait for
// response headers: short on the first attempt, so a stuck connection is
// retried while the player is still waiting, longer on the following ones.
// Vars so tests can shrink them.
var (
	segmentFirstAttemptTTFB = 7 * time.Second
	segmentRetryAttemptTTFB = 15 * time.Second
)

func segmentAttemptTTFB(attempt int) time.Duration {
	if attempt == 1 {
		return segmentFirstAttemptTTFB
	}
	return segmentRetryAttemptTTFB
}

type segFlight struct {
	done    chan struct{}
	waiters atomic.Int32
}

var segFlights sync.Map // upstream URL → *segFlight

// joinSegFlight returns the in-flight fetch for key, creating it (leader =
// true) when there is none. A follower is counted in waiters.
func joinSegFlight(key string) (f *segFlight, leader bool) {
	nf := &segFlight{done: make(chan struct{})}
	if v, loaded := segFlights.LoadOrStore(key, nf); loaded {
		f = v.(*segFlight)
		f.waiters.Add(1)
		return f, false
	}
	return nf, true
}

// finish ends the leader's flight and wakes the followers.
func (f *segFlight) finish(key string) {
	segFlights.CompareAndDelete(key, f)
	close(f.done)
}
