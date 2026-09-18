package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mycelium/internal/core"
)

// signedKeyURL builds a /proxy/key.key request for upstreamURL, direct
// egress (vpn=0), signed the same way media_handler.go/proxy.go's playlist
// rewriter would. Mirrors signedSegmentURL (proxy_challenge_test.go).
func signedKeyURL(upstreamURL string) string {
	return core.AppendProxySig("http://box.local/proxy/key.key?data=" + b64url(upstreamURL) + "&origin=&cookies=&vpn=0")
}

// useLoopbackPlaylistClient swaps getProxySession()'s underlying transport
// for a plain, unguarded http.Client for the duration of a test.
// upstreamPlaylistClient(useVPN=false, ...) returns getProxySession(), whose
// default transport (newUTLSTransport) blocks loopback via
// core.CheckURLNotSSRF — same reasoning as useLoopbackSegmentClient
// (proxy_challenge_test.go), just for the playlist/key client ProxyKey uses.
func useLoopbackPlaylistClient(t *testing.T) {
	t.Helper()
	upstreamMu.Lock()
	old := proxySession
	proxySession = &http.Client{}
	upstreamMu.Unlock()
	t.Cleanup(func() {
		upstreamMu.Lock()
		proxySession = old
		upstreamMu.Unlock()
	})
}

// TestProxySegment_RetryBudgetCapsTotalRetryTime is the regression test for
// bug 1: before segmentRetryBudget, ProxySegment's retry loop had no shared
// ceiling on the 4 attempts combined — a CDN that's slow (never failing
// outright, just never responding) on every attempt could burn up to
// maxAttempts×60s (the segment client's own per-attempt Timeout) plus
// backoff, worst case ~4-4.5 minutes, before finally giving up with a 502.
//
// segmentRetryBudget is shrunk to make the assertion run in milliseconds:
// the segment client is also swapped for a plain http.Client{} with NO
// Timeout of its own (useLoopbackSegmentClient), so the only thing that can
// possibly bound this call is budgetCtx — without the fix this test would
// hang indefinitely (neither the client nor r.Context(), which is never
// cancelled here, would ever time out), rather than merely run slow.
func TestProxySegment_RetryBudgetCapsTotalRetryTime(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)

	oldBudget := segmentRetryBudget
	segmentRetryBudget = 80 * time.Millisecond
	defer func() { segmentRetryBudget = oldBudget }()

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// Never respond — mimics a CDN so slow every attempt would otherwise
		// crawl toward its own client Timeout. Unblocks as soon as the
		// client cancels: budget expiry cancels the outbound request's
		// context, which tears down the connection and cancels this
		// handler's r.Context() too, so the test doesn't leak the goroutine.
		<-r.Context().Done()
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodGet, signedSegmentURL(upstream.URL+"/slow.ts"), nil)
	rec := httptest.NewRecorder()

	start := time.Now()
	ProxySegment(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s; want 502", rec.Code, rec.Body.String())
	}
	// Generous upper bound with headroom for slow CI schedulers, but nowhere
	// near maxAttempts(4)×segmentRetryBudget — which is what "retry into an
	// already-expired context anyway" would look like.
	if maxWant := segmentRetryBudget + 500*time.Millisecond; elapsed > maxWant {
		t.Errorf("elapsed = %s, want < %s (budget %s) — the retry loop must stop once the overall budget is spent, not keep retrying into an already-expired context", elapsed, maxWant, segmentRetryBudget)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 — once budgetCtx is expired every further attempt would fail instantly, so the loop must give up instead of burning through the remaining attempts", got)
	}
}

// TestProxyKey_PropagatesRequestContext is the regression test for bug 2:
// ProxyKey, unlike ProxySegment, never called req.WithContext(r.Context()),
// so a disconnected/cancelled player left the outbound key fetch running
// until the playlist/key client's own Timeout (15s) elapsed instead of being
// cancelled right away.
//
// Using an incoming request whose context is already cancelled makes this
// deterministic without waiting on any real timeout: a correctly wired fetch
// must fail before ever reaching the network (RoundTrip checks the context
// up front), so the fake upstream must see zero hits and the call must
// return near-instantly.
func TestProxyKey_PropagatesRequestContext(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackPlaylistClient(t)

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("0123456789abcdef"))
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already gone before ProxyKey builds the outbound request

	req := httptest.NewRequest(http.MethodGet, signedKeyURL(upstream.URL+"/key.key"), nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	start := time.Now()
	ProxyKey(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s; want 502 (the fetch must be aborted, not served)", rec.Code, rec.Body.String())
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %s, want a near-instant failure — ProxyKey must propagate r.Context() to the outbound request instead of letting it run to the client's own 15s Timeout", elapsed)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("upstream was hit %d time(s); want 0 — an already-cancelled request context must stop the fetch before it reaches the network", got)
	}
}
