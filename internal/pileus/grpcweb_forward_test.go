package pileus

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// startH2CServer runs handler behind a plaintext HTTP/2 (h2c) listener — the
// same protocol grpcH2Client (initGRPCWebBridge) speaks to grpcTargetPtr's
// addr when its tls field is false. Returns "host:port".
func startH2CServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h2c.NewHandler(handler, &http2.Server{})}
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return lis.Addr().String()
}

// setGRPCTarget points the bridge at addr/tls for the duration of the test,
// restoring whatever grpcTargetPtr held before (nil in the ordinary case,
// since Start() never runs in this test binary).
func setGRPCTarget(t *testing.T, addr string, tls bool) {
	t.Helper()
	orig := grpcTargetPtr.Load()
	grpcTargetPtr.Store(&grpcTarget{addr: addr, tls: tls})
	t.Cleanup(func() { grpcTargetPtr.Store(orig) })
}

// The bridge must: forward the body + an allowlisted subset of headers to the
// local gRPC server, pass DATA frames straight through unmodified (gRPC and
// gRPC-web share the same 5-byte frame format), and re-encode HTTP/2 trailers
// as the special 0x80-flagged gRPC-web trailer frame.
func TestServeGRPCWeb_ForwardsAndReframesTrailers(t *testing.T) {
	var gotBody string
	var gotAuth, gotTimeout, gotXProfile, gotCookie string

	addr := startH2CServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotTimeout = r.Header.Get("Grpc-Timeout")
		gotXProfile = r.Header.Get("X-Profile-Id")
		gotCookie = r.Header.Get("Cookie")

		w.Header().Set("Content-Type", "application/grpc+proto")
		w.Write([]byte("hello-grpc-frame")) //nolint:errcheck
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
	})

	setGRPCTarget(t, addr, false)
	initGRPCWebBridge()

	req := httptest.NewRequest(http.MethodPost, "/mycelium.AuthService/AuthorizeDevice",
		strings.NewReader("request-bytes"))
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("Authorization", "Bearer abc")
	req.Header.Set("Grpc-Timeout", "5S")
	req.Header.Set("X-Profile-Id", "profile-1")
	req.Header.Set("Cookie", "should-not-be-forwarded=1") // not authorization/grpc-timeout/x-*
	req.RemoteAddr = "203.0.113.7:54321"

	rec := httptest.NewRecorder()
	serveGRPCWeb(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (grpc-web always 200, status is in trailers): body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/grpc-web+proto" {
		t.Errorf("Content-Type = %q, want application/grpc-web+proto", ct)
	}
	if gotBody != "request-bytes" {
		t.Errorf("upstream got body %q, want %q", gotBody, "request-bytes")
	}
	if gotAuth != "Bearer abc" {
		t.Errorf("Authorization not forwarded: got %q", gotAuth)
	}
	if gotTimeout != "5S" {
		t.Errorf("Grpc-Timeout not forwarded: got %q", gotTimeout)
	}
	if gotXProfile != "profile-1" {
		t.Errorf("X-Profile-Id (x- prefixed) not forwarded: got %q", gotXProfile)
	}
	if gotCookie != "" {
		t.Errorf("Cookie was forwarded (%q) — only authorization/grpc-timeout/x-* should pass", gotCookie)
	}

	body := rec.Body.Bytes()
	if !strings.HasPrefix(string(body), "hello-grpc-frame") {
		t.Fatalf("data frame not passed through untouched: %q", body)
	}
	trailerFrame := body[len("hello-grpc-frame"):]
	wantPayload := "grpc-status: 0\r\n"
	if len(trailerFrame) != 5+len(wantPayload) {
		t.Fatalf("trailer frame length = %d, want %d (frame=% x)", len(trailerFrame), 5+len(wantPayload), trailerFrame)
	}
	if trailerFrame[0] != 0x80 {
		t.Errorf("trailer flag byte = %#x, want 0x80", trailerFrame[0])
	}
	if n := binary.BigEndian.Uint32(trailerFrame[1:5]); int(n) != len(wantPayload) {
		t.Errorf("trailer length prefix = %d, want %d", n, len(wantPayload))
	}
	if got := string(trailerFrame[5:]); got != wantPayload {
		t.Errorf("trailer payload = %q, want %q", got, wantPayload)
	}
}

// x-http-host / x-http-scheme are synthesised from X-Forwarded-* (or the
// request itself) only when the browser didn't already send them, and
// X-Forwarded-For is always overwritten with the observed socket peer — an
// inbound value here is unauthenticated and would let a browser forge its own
// per-IP rate-limit bucket (see server.go's pairingLimiter).
func TestServeGRPCWeb_SynthesisesHostSchemeAndOverwritesXFF(t *testing.T) {
	var gotHost, gotScheme, gotXFF string
	addr := startH2CServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Header.Get("X-Http-Host")
		gotScheme = r.Header.Get("X-Http-Scheme")
		gotXFF = r.Header.Get("X-Forwarded-For")
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
	})
	setGRPCTarget(t, addr, false)
	initGRPCWebBridge()

	req := httptest.NewRequest(http.MethodPost, "/mycelium.MediaPipeline/GetCatalog", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("X-Forwarded-Host", "app.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "1.2.3.4") // must be ignored/overwritten
	req.RemoteAddr = "198.51.100.9:9000"

	serveGRPCWeb(httptest.NewRecorder(), req)

	if gotHost != "app.example.com" {
		t.Errorf("X-Http-Host = %q, want app.example.com", gotHost)
	}
	if gotScheme != "https" {
		t.Errorf("X-Http-Scheme = %q, want https", gotScheme)
	}
	if gotXFF != "198.51.100.9" {
		t.Errorf("X-Forwarded-For = %q, want the observed peer 198.51.100.9 (client-supplied value must be ignored)", gotXFF)
	}
}

// No gRPC server registered yet (grpcTargetPtr nil) → 503, not a hang or a
// panic dialing an empty address.
func TestServeGRPCWeb_503WhenGRPCNotReady(t *testing.T) {
	orig := grpcTargetPtr.Load()
	grpcTargetPtr.Store(nil)
	t.Cleanup(func() { grpcTargetPtr.Store(orig) })

	req := httptest.NewRequest(http.MethodPost, "/mycelium.AuthService/AuthorizeDevice", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	rec := httptest.NewRecorder()
	serveGRPCWeb(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
