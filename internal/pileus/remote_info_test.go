package pileus

import (
	"context"
	"testing"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGetServerInfo(t *testing.T) {
	h := &AuthHandler{}
	authed := context.WithValue(context.Background(), ctxKeyDeviceID{}, "dev-1")

	// Unauthenticated: refused (the interceptor would refuse it first).
	if _, err := h.GetServerInfo(context.Background(), &gen.GetServerInfoRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no device: %v", err)
	}

	// Remote access off: empty answer, not an error.
	SetRemoteInfo(nil)
	resp, err := h.GetServerInfo(authed, &gen.GetServerInfoRequest{})
	if err != nil || resp.RemoteHost != "" || resp.RemoteGrpcPort != 0 {
		t.Fatalf("off: %v %+v", err, resp)
	}

	SetRemoteInfo(func() RemoteInfo { return RemoteInfo{Host: "100.101.102.103", FQDN: "mycelium.tail1234.ts.net"} })
	t.Cleanup(func() { SetRemoteInfo(nil) })
	resp, err = h.GetServerInfo(authed, &gen.GetServerInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.RemoteHost != "100.101.102.103" || resp.RemoteFqdn != "mycelium.tail1234.ts.net" || resp.RemoteGrpcPort != 50051 || resp.RemoteHttpPort != 8000 {
		t.Fatalf("on: %+v", resp)
	}
}
