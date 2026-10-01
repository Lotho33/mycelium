package pileus

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"mycelium/internal/core"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// grpcTarget is what the gRPC-web bridge needs to reach the gRPC server: the
// loopback address and whether it speaks TLS. Written once by Start, read by
// every bridged request (atomic).
type grpcTarget struct {
	addr string
	tls  bool
}

var grpcTargetPtr atomic.Pointer[grpcTarget]

// tlsFingerprint is the SHA-256 (hex) of the gRPC server's self-signed
// certificate, "" without TLS. /pileus/info hands it to clients, which pin
// it on first contact.
var tlsFingerprint string

// TLSFingerprint reports whether the gRPC server has TLS enabled and, if so,
// the SHA-256 fingerprint (hex-encoded) of its certificate.
func TLSFingerprint() (fingerprint string, enabled bool) {
	return tlsFingerprint, tlsFingerprint != ""
}

// Start launches the gRPC server on addr (e.g. ":50051") with AuthService,
// MediaPipeline and PluginService and the JWT auth interceptors, serving in
// the background. The returned server should be GracefulStop()ped on
// shutdown.
//
// tlsCert nil serves plaintext: bearer tokens and pairing codes then travel
// in the clear, acceptable only inside an already-encrypted network.
// Otherwise its fingerprint is published through TLSFingerprint for pinning.
func Start(addr string, jwtSecret []byte, tlsCert *tls.Certificate) *grpc.Server {
	authHandler := NewAuthHandler(jwtSecret)
	mediaHandler := NewMediaHandler()
	pluginHandler := NewPluginHandler()

	tlsEnabled := tlsCert != nil
	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(authInterceptor(authHandler)),
		grpc.StreamInterceptor(authStreamInterceptor(authHandler)),
	}
	if tlsCert != nil {
		opts = append(opts, grpc.Creds(credentials.NewServerTLSFromCert(tlsCert)))
		if len(tlsCert.Certificate) > 0 {
			sum := sha256.Sum256(tlsCert.Certificate[0])
			tlsFingerprint = hex.EncodeToString(sum[:])
		}
		log.Printf("[pileus] gRPC TLS abilitato (fingerprint %s)", tlsFingerprint)
	} else {
		log.Printf("[pileus] gRPC in CHIARO (TLS disabilitato) — JWT e credenziali viaggiano non cifrati: esporre questa porta SOLO dentro una rete già cifrata (Tailscale), mai direttamente")
	}
	srv := grpc.NewServer(opts...)
	gen.RegisterAuthServiceServer(srv, authHandler)
	gen.RegisterMediaPipelineServer(srv, mediaHandler)
	gen.RegisterPluginServiceServer(srv, pluginHandler)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("[pileus] listen %s: %v", addr, err)
	}
	// The bridge dials an explicit loopback host with the bound port
	// (lis.Addr() may be "[::]:<port>").
	var listenAddr string
	if ta, ok := lis.Addr().(*net.TCPAddr); ok {
		listenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(ta.Port))
	} else {
		listenAddr = lis.Addr().String()
	}
	grpcTargetPtr.Store(&grpcTarget{addr: listenAddr, tls: tlsEnabled})
	log.Printf("[pileus] gRPC server listening on %s (bridge dials %s, tls=%v)", lis.Addr().String(), listenAddr, tlsEnabled)
	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Printf("[pileus] server stopped: %v", err)
		}
	}()
	return srv
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth interceptors: AuthorizeDevice (first contact) is only rate-limited;
// every other RPC requires a valid device JWT.
// ─────────────────────────────────────────────────────────────────────────────

var publicMethods = map[string]bool{
	"/mycelium.AuthService/AuthorizeDevice": true,
}

// pairingLimiter throttles AuthorizeDevice per source IP, so nobody can burn
// a fresh pairing code with fast wrong guesses. Sized for humans.
var pairingLimiter = newIPRateLimiter(30, 30)

type ipRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*ipBucket
	rate     float64 // tokens per second
	capacity float64
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

func newIPRateLimiter(perMinute, burst float64) *ipRateLimiter {
	l := &ipRateLimiter{buckets: make(map[string]*ipBucket), rate: perMinute / 60.0, capacity: burst}
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			func() {
				defer core.Guard("pileus/ratelimiter-gc-tick")
				l.mu.Lock()
				defer l.mu.Unlock()
				cutoff := time.Now().Add(-10 * time.Minute)
				for ip, b := range l.buckets {
					if b.last.Before(cutoff) {
						delete(l.buckets, ip)
					}
				}
			}()
		}
	}()
	return l
}

func (l *ipRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[ip]
	if b == nil {
		b = &ipBucket{tokens: l.capacity, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.capacity {
		b.tokens = l.capacity
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// peerIP is the caller's IP for rate limiting. Requests from the in-process
// gRPC-web bridge arrive from loopback, so for a loopback peer the
// x-forwarded-for the bridge sets is used.
func peerIP(ctx context.Context) string {
	direct := "unknown"
	if pr, ok := peer.FromContext(ctx); ok && pr.Addr != nil {
		if host, _, err := net.SplitHostPort(pr.Addr.String()); err == nil {
			direct = host
		} else {
			direct = pr.Addr.String()
		}
	}
	if ip := net.ParseIP(direct); ip != nil && ip.IsLoopback() {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if xff := md.Get("x-forwarded-for"); len(xff) > 0 && xff[0] != "" {
				first, _, _ := strings.Cut(xff[0], ",")
				if first = strings.TrimSpace(first); first != "" {
					return first
				}
			}
		}
	}
	return direct
}

func authInterceptor(auth *AuthHandler) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := authenticate(ctx, auth, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// authStreamInterceptor applies the same checks to streaming RPCs (grpc-go
// never runs unary interceptors on streams).
func authStreamInterceptor(auth *AuthHandler) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := authenticate(ss.Context(), auth, info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &authedStream{ServerStream: ss, ctx: ctx})
	}
}

// authedStream overrides Context() so stream handlers see the device/profile
// values authenticate() injected.
type authedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authedStream) Context() context.Context { return s.ctx }

// authenticate validates the device JWT for fullMethod (public pairing
// methods are only rate-limited) and returns ctx enriched with the device
// and, when sent, the profile ID.
func authenticate(ctx context.Context, auth *AuthHandler, fullMethod string) (context.Context, error) {
	if publicMethods[fullMethod] {
		if !pairingLimiter.allow(peerIP(ctx)) {
			return nil, status.Error(codes.ResourceExhausted, "troppi tentativi di pairing, riprova tra poco")
		}
		return ctx, nil
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no metadata")
	}

	authVals := md.Get("authorization")
	if len(authVals) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing authorization header")
	}
	tokenStr := strings.TrimPrefix(authVals[0], "Bearer ")

	deviceID, err := auth.ParseJWT(tokenStr)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid token: "+err.Error())
	}

	// A valid signature is not enough: the device must still be paired and not
	// revoked.
	if !deviceActive(deviceID) {
		return nil, status.Error(codes.Unauthenticated, "device revoked or unknown — re-pair from the dashboard")
	}

	ctx = context.WithValue(ctx, ctxKeyDeviceID{}, deviceID)

	if sess := md.Get("x-profile-session"); len(sess) > 0 && sess[0] != "" {
		ctx = context.WithValue(ctx, ctxKeyProfileSession{}, sess[0])
	}

	// Profile ID sent by the client.
	if pid := md.Get("x-profile-id"); len(pid) > 0 && pid[0] != "" {
		// A PIN-protected profile needs this device's trust or a one-off session
		// token (profile_pin.go). AuthService is exempt: it lists and unlocks
		// profiles, and checks the profile named in each request itself.
		if !strings.HasPrefix(fullMethod, "/mycelium.AuthService/") {
			ok, err := profileAccessible(deviceID, pid[0], profileSessionFromCtx(ctx))
			if err != nil {
				return nil, status.Error(codes.Internal, "profile access check: "+err.Error())
			}
			if !ok {
				return nil, errProfileLocked
			}
		}
		ctx = context.WithValue(ctx, ctxKeyProfileID{}, pid[0])
	}
	return ctx, nil
}

// ctxKeyProfileID is the context key for the active profile ID.
type ctxKeyProfileID struct{}
