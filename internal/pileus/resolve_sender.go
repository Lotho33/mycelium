package pileus

import (
	"context"
	"sync"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
)

// resolveKeepAliveEvery is how long ResolveStream may stay silent before the
// last progress message is re-sent. Pileus aborts a resolve after 30s with no
// event at all, while a plugin's single blocking step (browser sniff, probe
// loop) can legitimately take longer. A var so tests can shrink it.
var resolveKeepAliveEvery = 10 * time.Second

// resolveSender serialises every Send on a ResolveStream: progress comes from
// the Lua goroutine (mycelium.progress), from the pre-buffer and from the
// keep-alive ticker, and grpc-go forbids concurrent Send on one stream. Once
// the result is sent or the handler returns (close), later sends — e.g. from
// a timed-out Lua goroutine still running in the background — are dropped
// instead of racing a torn-down stream.
type resolveSender struct {
	mu         sync.Mutex
	stream     gen.MediaPipeline_ResolveStreamServer
	closed     bool
	lastStatus string
	lastMsg    string
	lastSent   time.Time
}

func newResolveSender(stream gen.MediaPipeline_ResolveStreamServer) *resolveSender {
	return &resolveSender{stream: stream, lastMsg: "Ricerca della sorgente…", lastSent: time.Now()}
}

func (s *resolveSender) progress(status, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.lastStatus, s.lastMsg, s.lastSent = status, message, time.Now()
	return s.stream.Send(&gen.ResolveStreamEvent{
		Payload: &gen.ResolveStreamEvent_Progress{Progress: &gen.ResolveProgress{Message: message, Status: status}},
	})
}

func (s *resolveSender) result(r *gen.ResolveResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.stream.Send(&gen.ResolveStreamEvent{Payload: &gen.ResolveStreamEvent_Result{Result: r}})
}

func (s *resolveSender) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// keepAlive re-sends the last progress message whenever the stream has been
// silent for `every`, until ctx ends or the returned stop is called.
func (s *resolveSender) keepAlive(ctx context.Context, every time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every / 2)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				s.mu.Lock()
				if !s.closed && time.Since(s.lastSent) >= every {
					s.lastSent = time.Now()
					_ = s.stream.Send(&gen.ResolveStreamEvent{
						Payload: &gen.ResolveStreamEvent_Progress{Progress: &gen.ResolveProgress{Message: s.lastMsg, Status: s.lastStatus}},
					})
				}
				s.mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
