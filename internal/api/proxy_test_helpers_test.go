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

// useLoopbackSegmentClient swaps getSegmentClient()'s underlying transport
// for a plain, unguarded http.Client for the duration of a test.
// upstreamSegmentClient(useVPN=false, ...) returns getSegmentClient(), whose
// default transport (newUTLSTransport, cobweb profile by default) runs every
// fetch through core.CheckURLNotSSRF — which always blocks loopback, so an
// httptest.Server (127.0.0.1) never reaches its handler at all under the
// default transport. That's the guard working correctly; tests using this
// helper are about a different code path and need the fake upstream
// reachable, the same way a real CDN host would be in production.
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
