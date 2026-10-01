package api

import (
	"log"
	"net/http"
	"net/url"
	"strings"
)

// SameOriginMiddleware refuses state-changing requests to the dashboard
// (/admin/*) and the first-run setup (/setup/*) whose Origin — or Referer,
// when a browser sent no Origin — names a different host than the one the
// request was sent to.
//
// SameSite=Strict on the admin cookie is not enough on its own: for cookies
// "site" ignores the port, so any page served from another port of the same
// host (a media server, a sidecar's web UI, …) is same-site and its form
// POSTs carry the admin cookie. saveSettings reads JSON regardless of
// Content-Type, so a text/plain form could rewrite pileus_web_repo and then
// install attacker JavaScript on this origin. /setup/save has no cookie at
// all: after a factory reset any page open in a LAN browser could claim the
// admin account.
//
// Requests with neither header (curl, scripts) pass: without a browser there
// is no ambient cookie to abuse.
func SameOriginMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if csrfGuarded(r) && !sameOriginRequest(r) {
			log.Printf("[csrf] rifiutata %s %s da origine %q (host %q, ip %s)",
				r.Method, r.URL.Path, requestOrigin(r), r.Host, realIP(r))
			http.Error(w, "origine della richiesta non consentita", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func csrfGuarded(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	p := r.URL.Path
	return strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/setup/")
}

// requestOrigin is the Origin header, else the scheme+host of Referer.
func requestOrigin(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" {
		return o
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
		return "invalid"
	}
	return ""
}

func sameOriginRequest(r *http.Request) bool {
	origin := requestOrigin(r)
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" { // includes "null" (sandboxed frames, data: pages)
		return false
	}
	host := r.Host
	// Behind a trusted reverse proxy that rewrites Host, the browser-facing
	// one is in X-Forwarded-Host.
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" && isTrustedProxy(directRemoteHost(r)) {
		host, _, _ = strings.Cut(fh, ",")
		host = strings.TrimSpace(host)
	}
	return strings.EqualFold(u.Host, host)
}
