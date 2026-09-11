package sdk

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrNotFound signals that the requested content does not exist.
// Maps to gRPC codes.NotFound → HTTP 404.
func ErrNotFound(msg string) error { return status.Error(codes.NotFound, msg) }

// ErrUnavailable signals a transient upstream failure (rate-limit, 503).
// Maps to gRPC codes.Unavailable → HTTP 503.
func ErrUnavailable(msg string) error { return status.Error(codes.Unavailable, msg) }

// ErrBadConfig signals missing or invalid plugin configuration.
// Maps to gRPC codes.FailedPrecondition → HTTP 400.
func ErrBadConfig(msg string) error { return status.Error(codes.FailedPrecondition, msg) }
