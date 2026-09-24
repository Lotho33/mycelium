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

// grpcTarget bundles what the gRPC-web bridge needs to reach the native gRPC
// server: the loopback address to dial and whether it speaks TLS (so the
// bridge dials it with the right scheme). Set once by Start(), read on every
// bridged request from a different goroutine — atomic.Pointer instead of two
// plain package vars avoids both the data race (Start()'s write and
// serveGRPCWeb's read have no happens-before edge the Go memory model
// guarantees) and a window where one field is updated and the other isn't.
type grpcTarget struct {
	addr string
	tls  bool
}

var grpcTargetPtr atomic.Pointer[grpcTarget]

// tlsFingerprint is the SHA-256 (hex) of the gRPC server's leaf certificate,
// empty when TLS is disabled. Exposed via TLSFingerprint() so internal/api's
// unauthenticated /pileus/info discovery endpoint can hand it to a Pileus
// client on first contact — the client pins this value (TOFU) since the
// cert is self-signed on purpose (see GenerateOrLoadTLSCert) and has no
// public CA to validate against.
var tlsFingerprint string

// TLSFingerprint reports whether the gRPC server has TLS enabled and, if so,
// the SHA-256 fingerprint (hex-encoded) of its certificate.
func TLSFingerprint() (fingerprint string, enabled bool) {
	return tlsFingerprint, tlsFingerprint != ""
}

// Start launches the Pileus gRPC server on the given address (e.g. ":50051").
// It registers AuthService, MediaPipeline, and PluginService, wires the JWT auth
// interceptor, and serves in the background. Returns the *grpc.Server so the
// caller can GracefulStop() it on shutdown — without that, process exit tears
// down open connections with a bare TCP RST instead of a clean HTTP/2 GOAWAY,
// which frontend clients see as an abrupt "connection forcefully terminated"
// error (HTTP/2 error 10) rather than a normal disconnect.
//
// tlsCert: nil = plaintext (comportamento legacy, per compatibilità con
// build client precedenti a questo commit — vedi il commento su
// GenerateOrLoadTLSCert). Non-nil = TLS abilitato con quel certificato
// self-signed; il suo fingerprint viene pubblicato via TLSFingerprint() così
// /pileus/info (internal/api, endpoint HTTP non autenticato già interrogato
// dal client Pileus in fase di discovery) può darlo al client per il pinning
// TOFU, dato che è self-signed e non c'è una CA da validare. Senza TLS, i
// JWT bearer e il payload di AuthorizeDevice (incluso PinHash, che il client
// Pileus manda in chiaro nonostante il nome del campo — verificato dal vivo
// leggendo il repo Flutter il 2026-08-19) viaggiano in chiaro sulla rete:
// accettabile SOLO se questa porta resta raggiungibile esclusivamente dentro
// un tunnel già cifrato (Tailscale), mai esposta direttamente.
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
	// The gRPC-web bridge connects here from inside this same process. Use an
	// explicit loopback host with the actually-bound port — NOT lis.Addr()
	// verbatim, which on a dual-stack listener is "[::]:<port>" and dialing the
	// unspecified address is unreliable.
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
// JWT auth interceptor
// Skips auth for AuthorizeDevice (first contact).
// All other RPCs require a valid Bearer JWT.
// ─────────────────────────────────────────────────────────────────────────────

var publicMethods = map[string]bool{
	"/mycelium.AuthService/AuthorizeDevice": true,
}

// pairingLimiter throttles the unauthenticated first-contact RPC
// (AuthorizeDevice) per source IP. Without it anyone who can reach this port can
// spend every freshly issued pairing code in milliseconds — 5 wrong PINs and
// the code is dead — a cheap denial of service on pairing. Sized for humans: a
// real client retries a handful of times, never dozens per minute.
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

// peerIP is the caller's source IP for rate-limiting. On the gRPC-web path every
// request arrives from the in-process bridge at 127.0.0.1, so when the direct
// peer is loopback we trust an x-forwarded-for the bridge (or a local reverse
// proxy) attached; otherwise it's the raw peer address.
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

// authStreamInterceptor applies the same checks as authInterceptor to
// streaming RPCs. grpc-go never runs unary interceptors on streams: without
// this, ResolveStream (server-streaming) answered anonymous callers with
// signed /proxy URLs — and, for direct_stream plugins, the raw upstream URL
// plus its auth headers.
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

	// A valid signature is not enough: the device must still be paired and
	// not revoked (P1-4 — a 30-day JWT is otherwise unrevocable).
	if !deviceActive(deviceID) {
		return nil, status.Error(codes.Unauthenticated, "device revoked or unknown — re-pair from the dashboard")
	}

	ctx = context.WithValue(ctx, ctxKeyDeviceID{}, deviceID)

	// Inject profile ID if provided by the client.
	if pid := md.Get("x-profile-id"); len(pid) > 0 && pid[0] != "" {
		ctx = context.WithValue(ctx, ctxKeyProfileID{}, pid[0])
	}
	return ctx, nil
}

// ctxKeyProfileID is the context key for the active profile ID.
type ctxKeyProfileID struct{}
