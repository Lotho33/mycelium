# Security policy

## Reporting a vulnerability

Please report suspected security issues privately via GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
("Report a vulnerability" on the repository's **Security** tab), not as a public
issue.

Include what you can: affected version/commit, a description, and repro steps or
a proof of concept. Please give a reasonable window to respond and ship a fix
before any public disclosure.

## Scope

In scope: the mycelium runtime in this repository — the HTTP/gRPC APIs, admin
dashboard, HLS proxy, client authentication, and the plugin ZIP-upload path.

Out of scope: third-party plugins (report to the plugin's own maintainer), the
optional companion browser service (separate repo), and issues that require an
already-compromised host or admin credentials.

## Hardening notes

- Admin session cookies are `HttpOnly`, `SameSite=Strict`, short-lived.
- The settings write API enforces a key allowlist.
- Plugin ZIP upload has path-traversal and extracted-size limits.
- Hub API tokens are HMAC-SHA256 with a short validity window; login is
  rate-limited. Unauthenticated gRPC first-contact (device pairing) is
  rate-limited per source IP.
- Each plugin runs in its own Lua VM, so a plugin fault never takes down the
  core. This is **fault isolation, not a privilege sandbox**: mycelium is
  single-tenant and the operator is trusted to vet the plugins they install.
  The Lua standard library is trimmed (no `os.execute`/`os.exit`/`io`/`debug`,
  no dynamic code or native-library loading) as damage limitation, not as a
  boundary against code that already runs in the process.
- `/proxy/*` URLs are HMAC-signed by the server, so they can't be forged or
  pointed at arbitrary hosts. `/proxy/*` and `/img` dial upstreams through an
  SSRF guard (no loopback, link-local or metadata addresses) and are rate
  limited per IP. `/img` only returns bytes that decode as a raster image.
- The unauthenticated first-run setup is accepted only from the local network.
  The admin dashboard requires a login, and state-changing admin requests are
  checked against the request origin (CSRF).
- mycelium is meant for a private network. To expose it publicly, put it behind
  an HTTPS reverse proxy and keep `/admin` and `/setup` private.
