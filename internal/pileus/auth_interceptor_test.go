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

// A syntactically valid, correctly signed token for a device that is no
// longer active (revoked / never paired) must still be rejected — P1-4, a
// valid signature alone isn't enough.
func TestAuthInterceptor_RejectsRevokedDevice(t *testing.T) {
	h := testAuthHandler()
	it := authInterceptor(h)
	info := &grpc.UnaryServerInfo{FullMethod: "/mycelium.MediaPipeline/GetCatalog"}

	orig := deviceActiveLookup
	deviceActiveLookup = func(id string) bool { return id != "dev-revoked" }
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
	h := testAuthHandler()
	it := authInterceptor(h)
	info := &grpc.UnaryServerInfo{FullMethod: "/mycelium.MediaPipeline/GetCatalog"}

	orig := deviceActiveLookup
	deviceActiveLookup = func(string) bool { return true }
	t.Cleanup(func() { deviceActiveLookup = orig })
	deviceActiveCache.Delete("dev-ok")

	tok, err := h.mintJWT("dev-ok", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+tok, "x-profile-id", "prof-9"))

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
	if gotProfile != "prof-9" {
		t.Errorf("profile id in context = %q, want prof-9", gotProfile)
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
