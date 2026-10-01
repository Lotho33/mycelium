package pileus

import (
	"context"
	"sync"
	"testing"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc"
)

type recordingResolveStream struct {
	grpc.ServerStream
	mu     sync.Mutex
	events []*gen.ResolveStreamEvent
}

func (r *recordingResolveStream) Send(e *gen.ResolveStreamEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recordingResolveStream) snapshot() []*gen.ResolveStreamEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*gen.ResolveStreamEvent(nil), r.events...)
}

// A plugin step that stays silent longer than the keep-alive period must still
// produce events (Pileus gives up after 30s of silence), repeating the last
// message rather than an empty one.
func TestResolveSender_KeepAliveRepeatsLastMessage(t *testing.T) {
	rs := &recordingResolveStream{}
	s := newResolveSender(rs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := s.keepAlive(ctx, 40*time.Millisecond)
	defer stop()

	_ = s.progress("", "Carico l'embed nel browser…")
	time.Sleep(150 * time.Millisecond)

	evs := rs.snapshot()
	if len(evs) < 2 {
		t.Fatalf("got %d events, want the progress plus at least one keep-alive", len(evs))
	}
	for _, e := range evs[1:] {
		if got := e.GetProgress().GetMessage(); got != "Carico l'embed nel browser…" {
			t.Fatalf("keep-alive message = %q, want the last progress message", got)
		}
	}
}

// After the result (or handler exit) nothing else may be sent — e.g. a
// timed-out Lua goroutine calling mycelium.progress late.
func TestResolveSender_NoSendsAfterResult(t *testing.T) {
	rs := &recordingResolveStream{}
	s := newResolveSender(rs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := s.keepAlive(ctx, 20*time.Millisecond)
	defer stop()

	_ = s.result(&gen.ResolveResponse{ResolvedUrl: "http://x"})
	_ = s.progress("", "late")
	time.Sleep(80 * time.Millisecond)

	evs := rs.snapshot()
	if len(evs) != 1 || evs[0].GetResult() == nil {
		t.Fatalf("events = %v, want exactly the result", evs)
	}
}
