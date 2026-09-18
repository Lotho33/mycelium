package sdk

import "net/http"

// NewHTTPClient returns a standard http.Client for use in native (gRPC) plugins.
// Native plugins make outbound requests with the standard library directly;
// this is just a convenience constructor.
func NewHTTPClient() *http.Client {
	return &http.Client{}
}
