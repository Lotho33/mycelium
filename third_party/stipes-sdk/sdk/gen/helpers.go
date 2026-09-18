package gen

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrNotReady is returned when Setup reports the plugin is not configured.
var ErrNotReady = status.Error(codes.FailedPrecondition, "plugin not ready")

// WrapError converts a plain Go error to a gRPC status error.
func WrapError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}

// UnwrapError extracts the message from a gRPC status error (for logging).
func UnwrapError(err error) string {
	if err == nil {
		return ""
	}
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}
	return err.Error()
}

