package api

import "sync"

// playlistInflight deduplicates concurrent upstream HLS playlist fetches.
// Multiple goroutines requesting the same URL share a single upstream HTTP call:
// the first caller fetches, subsequent concurrent callers wait and receive the
// same bytes. This prevents N upstream requests when N users watch the same
// live channel simultaneously.
type inflightEntry struct {
	done   chan struct{}
	result []byte
	err    error
}

type inflightMap struct {
	mu       sync.Mutex
	inflight map[string]*inflightEntry
}

var playlistInflight = &inflightMap{
	inflight: make(map[string]*inflightEntry),
}

// Do executes fn for key if no call is already in flight, otherwise waits for
// the in-flight call and returns its result. The bool is true when the result
// was shared from another caller.
func (m *inflightMap) Do(key string, fn func() ([]byte, error)) ([]byte, error, bool) {
	m.mu.Lock()
	if e, ok := m.inflight[key]; ok {
		m.mu.Unlock()
		<-e.done
		return e.result, e.err, true
	}
	e := &inflightEntry{done: make(chan struct{})}
	m.inflight[key] = e
	m.mu.Unlock()

	e.result, e.err = fn()
	close(e.done)

	m.mu.Lock()
	delete(m.inflight, key)
	m.mu.Unlock()

	return e.result, e.err, false
}
