package api

import (
	"net/http"
	"testing"

	"mycelium/internal/core"
)

// signedSegmentURL builds a /proxy/segment.ts request for upstreamURL,
// direct egress (vpn=0), signed the same way media_handler.go/proxy.go's
// playlist rewriter would. Shared by tests that exercise the segment proxy
// path (proxy_retry_budget_test.go and friends).
func signedSegmentURL(upstreamURL string) string {
	return core.AppendProxySig("http://box.local/proxy/segment.ts?data=" + b64url(upstreamURL) + "&origin=&cookies=&vpn=0")
}

// useLoopbackSegmentClient swaps getSegmentClient()'s transport for a plain
// client for the test: the default one's SSRF guard blocks loopback, where
// httptest servers listen.
func useLoopbackSegmentClient(t *testing.T) {
	t.Helper()
	upstreamMu.Lock()
	old := segmentClient
	segmentClient = &http.Client{}
	upstreamMu.Unlock()
	t.Cleanup(func() {
		upstreamMu.Lock()
		segmentClient = old
		upstreamMu.Unlock()
	})
}
