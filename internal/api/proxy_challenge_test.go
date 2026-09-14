package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"mycelium/internal/core"
	"mycelium/internal/managers"
)

// challengeHitsFor returns the current Hits count for the (domain, egress)
// pair PendingChallenges() reports, or 0 if there's no entry yet — lets a
// test assert "exactly one new hit landed" without needing a way to reset
// the package-level challenge registry between tests.
func challengeHitsFor(t *testing.T, domain, egress string) int {
	t.Helper()
	for _, c := range managers.PendingChallenges() {
		if c.Domain == domain && c.Egress == egress {
			return c.Hits
		}
	}
	return 0
}

// signedSegmentURL builds a /proxy/segment.ts request for upstreamURL,
// direct egress (vpn=0), signed the same way media_handler.go/proxy.go's
// playlist rewriter would.
func signedSegmentURL(upstreamURL string) string {
	return core.AppendProxySig("http://box.local/proxy/segment.ts?data=" + b64url(upstreamURL) + "&origin=&cookies=&vpn=0")
}

// useLoopbackSegmentClient swaps getSegmentClient()'s underlying transport
// for a plain, unguarded http.Client for the duration of a test.
// upstreamSegmentClient(useVPN=false, ...) returns getSegmentClient(), whose
// default transport (newUTLSTransport, cobweb profile by default) runs every
// fetch through core.CheckURLNotSSRF — which always blocks loopback, so an
// httptest.Server (127.0.0.1) never reaches its handler at all under the
// default transport. That's the guard working correctly; these tests are
// about a different code path (noteChallengeIfAny) and need the fake
// upstream reachable to exercise it, the same way a real CDN host would be
// for these handlers in production.
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

// TestProxySegment_RecordsChallengeOnMislabelledCloudflareResponse is the
// regression test for the gap reported by the user (14/09): a CDN can
// challenge an individual *segment* fetch while the playlist itself sails
// through — Cloudflare typically does this as a plain 200 OK carrying an
// html body and Cf-Mitigated: challenge, which ProxySegment already detected
// (via its media-signature sniff) and correctly rejected as "non-video
// segment" — but never recorded via noteChallengeIfAny, so it never reached
// the dashboard's pending-challenges list, only raw logs.
func TestProxySegment_RecordsChallengeOnMislabelledCloudflareResponse(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>Checking your browser before accessing...</body></html>"))
	}))
	defer upstream.Close()

	before := challengeHitsFor(t, "0.1", "direct") // registrableHost("127.0.0.1") == "0.1"

	req := httptest.NewRequest(http.MethodGet, signedSegmentURL(upstream.URL+"/seg1.ts"), nil)
	rec := httptest.NewRecorder()
	ProxySegment(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s; want 502 (the sniff must still reject this as non-video)", rec.Code, rec.Body.String())
	}
	after := challengeHitsFor(t, "0.1", "direct")
	if after != before+1 {
		t.Errorf("challenge hits for (0.1, direct) = %d, want %d — the 200-but-Cf-Mitigated case must be recorded via noteChallengeIfAny", after, before+1)
	}
}

// TestProxySegment_RecordsChallengeOnNon2xxCloudflareResponse covers the
// other call site added by the same fix: a segment fetch that fails outright
// (non-2xx) with Cf-Mitigated set. Uses 502 (not 403/429) so segRetryBackoff
// takes the fast ~150ms/300ms path instead of the multi-second 403 backoff —
// this test only cares that the challenge gets recorded, not about the retry
// timing itself.
func TestProxySegment_RecordsChallengeOnNon2xxCloudflareResponse(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	before := challengeHitsFor(t, "0.1", "direct")

	req := httptest.NewRequest(http.MethodGet, signedSegmentURL(upstream.URL+"/seg2.ts"), nil)
	rec := httptest.NewRecorder()
	ProxySegment(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s; want 502", rec.Code, rec.Body.String())
	}
	after := challengeHitsFor(t, "0.1", "direct")
	if after <= before {
		t.Errorf("challenge hits for (0.1, direct) = %d, want > %d — a non-2xx Cf-Mitigated response must be recorded", after, before)
	}
}

// TestProxySegment_NoChallengeRecordedWithoutCfMitigatedHeader is the
// negative counterpart: a generic non-media error page WITHOUT the
// Cf-Mitigated header (i.e. not actually a Cloudflare challenge — just some
// other upstream failure) must still be rejected as non-video, but must NOT
// be recorded as a pending challenge — noteChallengeIfAny's own header check
// already guarantees this; this test guards against a future edit at either
// of the two new call sites accidentally dropping that gate.
func TestProxySegment_NoChallengeRecordedWithoutCfMitigatedHeader(t *testing.T) {
	core.SetProxySignKey([]byte("proxy-sig-test-master"))
	defer core.SetProxySignKey(nil)
	useLoopbackSegmentClient(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>404 not found</body></html>"))
	}))
	defer upstream.Close()

	before := challengeHitsFor(t, "0.1", "direct")

	req := httptest.NewRequest(http.MethodGet, signedSegmentURL(upstream.URL+"/seg3.ts"), nil)
	rec := httptest.NewRecorder()
	ProxySegment(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body=%s; want 502", rec.Code, rec.Body.String())
	}
	after := challengeHitsFor(t, "0.1", "direct")
	if after != before {
		t.Errorf("challenge hits for (0.1, direct) went from %d to %d without a Cf-Mitigated header present", before, after)
	}
}
