package core

import "net/http"

// SetCORSHeaders writes the cross-origin headers every mycelium HTTP surface
// uses: wildcard origin, no credentials. This was duplicated (cmd/server's
// hub-API middleware, internal/pileus's gRPC-web bridge) with the "why
// wildcard is safe here" reasoning copy-pasted in both places — centralised
// so that reasoning has one home and can't drift out of sync between the two.
//
// The wildcard is deliberate, not an oversight: every endpoint here is either
// a token-authenticated API consumed by native apps (Kodi addons, Pileus —
// not browsers, so there's no cookie-based session for an Origin check to
// protect) or an admin route behind a SameSite=Strict session cookie (never
// sent cross-origin by a browser regardless of what CORS allows). methods and
// allowHeaders are route-specific; exposeHeaders is optional — only the
// gRPC-web bridge needs it, to expose the grpc-status/grpc-message trailers
// gRPC-web smuggles through as regular headers on error.
func SetCORSHeaders(w http.ResponseWriter, methods, allowHeaders, exposeHeaders string) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", methods)
	h.Set("Access-Control-Allow-Headers", allowHeaders)
	if exposeHeaders != "" {
		h.Set("Access-Control-Expose-Headers", exposeHeaders)
	}
}
