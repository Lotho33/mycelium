package pileus

import (
	"context"
	"testing"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Any paired device used to be able to rename every other device.
func TestRenameDevice_OnlySelf(t *testing.T) {
	h := testAuthHandler()
	ctx := context.WithValue(context.Background(), ctxKeyDeviceID{}, "dev-a")
	_, err := h.RenameDevice(ctx, &gen.RenameDeviceRequest{DeviceId: "dev-b", Label: "x"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("renaming another device: err = %v, want PermissionDenied", err)
	}
}
