package core

import (
	"encoding/base64"
	"strings"
)

// ServerURLPrefix marks a URL relative to this mycelium server in data a
// plugin hands out (an item's poster, an extra field…). A plugin can't know
// how each client reaches the server — LAN address, domain, gRPC-web origin
// — so the gRPC layer replaces the prefix with that client's own base URL on
// the way out (pileus/server_urls.go). Caches keep the relative form.
const ServerURLPrefix = "mycelium:/"

// IsServerURL reports whether s is a server-relative URL.
func IsServerURL(s string) bool { return strings.HasPrefix(s, ServerURLPrefix) }

// AbsoluteServerURL turns a server-relative URL into an absolute one under
// base ("https://host:port", no trailing slash); other strings are returned
// unchanged.
func AbsoluteServerURL(s, base string) string {
	if !IsServerURL(s) {
		return s
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimPrefix(s, ServerURLPrefix)
}

// VersusCoverURL is the server-relative URL of the 16:9 cover built from two
// badges (GET /img/versus).
func VersusCoverURL(homeBadge, awayBadge string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return ServerURLPrefix + "img/versus?h=" + enc([]byte(homeBadge)) + "&a=" + enc([]byte(awayBadge))
}
