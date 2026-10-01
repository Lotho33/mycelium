package pileus

import (
	"context"
	"testing"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"mycelium/internal/managers"
)

// The refusal uses ABORTED (reserved for "playing elsewhere", so the client
// can offer "watch here") and names the device.
func TestPlaybackInUseError(t *testing.T) {
	setupPinTestDB(t)
	err := playbackInUseError("tv")
	st, _ := status.FromError(err)
	if st.Code() != codes.Aborted || st.Message() != "questo profilo è già in riproduzione su «TV salotto»" {
		t.Fatalf("got %v %q", st.Code(), st.Message())
	}
	if DeviceDisplayName("unknown-device") != "un altro dispositivo" {
		t.Fatal("unlabelled device name")
	}
}

func TestReleasePlayback(t *testing.T) {
	setupPinTestDB(t)
	defer managers.PlaybackLeases.Release("me")
	managers.PlaybackLeases.Claim("me", "tv")
	h := &MediaHandler{}
	ctx := context.WithValue(context.WithValue(context.Background(), ctxKeyDeviceID{}, "tv"), ctxKeyProfileID{}, "me")
	if _, err := h.ReleasePlayback(ctx, &gen.ReleasePlaybackRequest{}); err != nil {
		t.Fatal(err)
	}
	if holder := managers.PlaybackLeases.Holder("me", "phone"); holder != "" {
		t.Fatalf("still held by %q after release", holder)
	}
}
