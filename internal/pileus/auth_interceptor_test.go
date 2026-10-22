package pileus

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func requireUnauthenticated(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if s, ok := status.FromError(err); !ok || s.Code() != codes.Unauthenticated {
		t.Fatalf("error = %v, want codes.Unauthenticated", err)
	}
}

func okHandler(ctx context.Context, req any) (any, error) { return "ok", nil }

func TestAuthInterceptor_RejectsMissingMetadata(t *testing.T) {
	it := authInterceptor(testAuthHandler())
	info := &grpc.UnaryServerInfo{FullMethod: "/mycelium.MediaPipeline/GetCatalog"}
	_, err := it(context.Background(), nil, info, okHandler)
	requireUnauthenticated(t, err)
}

func TestAuthInterceptor_RejectsMissingAuthorizationHeader(t *testing.T) {
	it := authInterceptor(testAuthHandler())
	info := &grpc.UnaryServerInfo{FullMethod: "/mycelium.MediaPipeline/GetCatalog"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})
	_, err := it(ctx, nil, info, okHandler)
	requireUnauthenticated(t, err)
}

func TestAuthInterceptor_RejectsInvalidToken(t *testing.T) {
	it := authInterceptor(testAuthHandler())
	info := &grpc.UnaryServerInfo{FullMethod: "/mycelium.MediaPipeline/GetCatalog"}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer not-a-jwt"))
	_, err := it(ctx, nil, info, okHandler)
	requireUnauthenticated(t, err)
}

// A correctly signed token of a device that is no longer active (revoked or
// never paired) is rejected.
func TestAuthInterceptor_RejectsRevokedDevice(t *testing.T) {
	h := testAuthHandler()
	it := authInterceptor(h)
	info := &grpc.UnaryServerInfo{FullMethod: "/mycelium.MediaPipeline/GetCatalog"}

	orig := deviceActiveLookup
	deviceActiveLookup = func(id string) (bool, string) { return id != "dev-revoked", "" }
	t.Cleanup(func() { deviceActiveLookup = orig })
	deviceActiveCache.Delete("dev-revoked")

	tok, err := h.mintJWT("dev-revoked", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
	_, err = it(ctx, nil, info, okHandler)
	requireUnauthenticated(t, err)
}

// A valid token for an active device passes through, and the handler sees the
// device (and, if sent, profile) ID via context — the whole point of the
// interceptor.
func TestAuthInterceptor_PassesDeviceAndProfileIDToHandler(t *testing.T) {
	setupPinTestDB(t) // the profile PIN check reads the DB
	h := testAuthHandler()
	it := authInterceptor(h)
	info := &grpc.UnaryServerInfo{FullMethod: "/mycelium.MediaPipeline/GetCatalog"}

	orig := deviceActiveLookup
	deviceActiveLookup = func(string) (bool, string) { return true, "" }
	t.Cleanup(func() { deviceActiveLookup = orig })
	deviceActiveCache.Delete("dev-ok")

	tok, err := h.mintJWT("dev-ok", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+tok, "x-profile-id", "fam"))

	var gotDevice, gotProfile string
	handler := func(ctx context.Context, req any) (any, error) {
		gotDevice, _ = ctx.Value(ctxKeyDeviceID{}).(string)
		gotProfile, _ = ctx.Value(ctxKeyProfileID{}).(string)
		return "ok", nil
	}
	if _, err := it(ctx, nil, info, handler); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDevice != "dev-ok" {
		t.Errorf("device id in context = %q, want dev-ok", gotDevice)
	}
	if gotProfile != "fam" {
		t.Errorf("profile id in context = %q, want fam", gotProfile)
	}
}

// An x-profile-id that names no profile (e.g. another device's id, which
// owns that device's profile-less data) is rejected.
func TestAuthInterceptor_RejectsUnknownProfile(t *testing.T) {
	setupPinTestDB(t)
	h := testAuthHandler()
	it := authInterceptor(h)
	info := &grpc.UnaryServerInfo{FullMethod: "/mycelium.MediaPipeline/ListDownloads"}

	orig := deviceActiveLookup
	deviceActiveLookup = func(string) (bool, string) { return true, "" }
	t.Cleanup(func() { deviceActiveLookup = orig })
	deviceActiveCache.Delete("phone")

	tok, err := h.mintJWT("phone", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+tok, "x-profile-id", "tv"))
	_, err = it(ctx, nil, info, okHandler)
	if s, ok := status.FromError(err); !ok || s.Code() != codes.NotFound {
		t.Fatalf("error = %v, want codes.NotFound", err)
	}
}

// AuthorizeDevice (first contact, no token yet) bypasses the JWT check
// entirely but still goes through the pairing rate limiter.
func TestAuthInterceptor_PublicMethodBypassesAuth(t *testing.T) {
	it := authInterceptor(testAuthHandler())
	info := &grpc.UnaryServerInfo{FullMethod: "/mycelium.AuthService/AuthorizeDevice"}
	called := false
	handler := func(ctx context.Context, req any) (any, error) { called = true; return nil, nil }
	if _, err := it(context.Background(), nil, info, handler); err != nil {
		t.Fatalf("public method rejected: %v", err)
	}
	if !called {
		t.Fatal("handler was not invoked for a public method")
	}
}

// fakeServerStream is the minimal grpc.ServerStream the stream interceptor
// needs: only Context() is consulted.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

// ResolveStream is server-streaming: grpc-go never runs unary interceptors on
// it, so the stream interceptor is its only auth gate.
func TestAuthStreamInterceptor_RejectsMissingAuthorization(t *testing.T) {
	it := authStreamInterceptor(testAuthHandler())
	info := &grpc.StreamServerInfo{FullMethod: "/mycelium.MediaPipeline/ResolveStream", IsServerStream: true}
	called := false
	handler := func(srv any, ss grpc.ServerStream) error { called = true; return nil }
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})
	err := it(nil, &fakeServerStream{ctx: ctx}, info, handler)
	requireUnauthenticated(t, err)
	if called {
		t.Fatal("handler invoked for an unauthenticated stream")
	}
}

func TestAuthStreamInterceptor_PassesDeviceAndProfileIDToHandler(t *testing.T) {
	setupPinTestDB(t) // the profile PIN check reads the DB
	h := testAuthHandler()
	it := authStreamInterceptor(h)
	info := &grpc.StreamServerInfo{FullMethod: "/mycelium.MediaPipeline/ResolveStream", IsServerStream: true}

	orig := deviceActiveLookup
	deviceActiveLookup = func(string) (bool, string) { return true, "" }
	t.Cleanup(func() { deviceActiveLookup = orig })
	deviceActiveCache.Delete("dev-stream")

	tok, err := h.mintJWT("dev-stream", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+tok, "x-profile-id", "me"))

	var gotDevice, gotProfile string
	handler := func(srv any, ss grpc.ServerStream) error {
		gotDevice, _ = ss.Context().Value(ctxKeyDeviceID{}).(string)
		gotProfile, _ = ss.Context().Value(ctxKeyProfileID{}).(string)
		return nil
	}
	if err := it(nil, &fakeServerStream{ctx: ctx}, info, handler); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDevice != "dev-stream" || gotProfile != "me" {
		t.Errorf("context = (%q, %q), want (dev-stream, me)", gotDevice, gotProfile)
	}
}

// A panicking handler answers Internal instead of crashing the server.
func TestRecoverUnary(t *testing.T) {
	_, err := recoverUnary(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x/Y"},
		func(context.Context, any) (any, error) { panic("boom") })
	if s, ok := status.FromError(err); !ok || s.Code() != codes.Internal {
		t.Fatalf("err = %v, want codes.Internal", err)
	}
}
