# Serving the Pileus web app (PWA)

mycelium exposes everything the Pileus Flutter **web** build needs over plain
HTTP/1.1, so it works from a browser with no native-gRPC support. This doc covers
what the browser talks to and what an operator must configure for it to actually
play video.

## What the browser uses

| Surface | Path | Notes |
|---|---|---|
| gRPC-web bridge | `POST /grpc/<pkg.Service>/<Method>` | Hand-rolled bridge in `internal/pileus/grpcweb.go` — no external deps. Also answered at the bare method path (`POST /mycelium.AuthService/Login`) for grpc-web clients that drop the `/grpc` segment when resolving the method URL. |
| HLS proxy | `GET /proxy/playlist.m3u8?…` / `GET /proxy/segment.ts?…` | Not auth-gated. All auth/context is **in the query string** (`data`, `origin`, `cookies`, `xhdr`, `sid`, `uid`, `vpn`/`egr`) so a bare `<video src>` works — no custom request headers. Responses carry `Access-Control-Allow-Origin: *`. |
| Image proxy | `GET /img?u=<b64 url>` | Re-encodes to WebP, disk-cached, fail-open (302 to origin). |

The gRPC-web bridge:

- speaks h2c **or** h2+TLS to the in-process gRPC server depending on whether
  that listener has TLS (`pileus_grpc_tls`, on by default) — the loopback hop
  skips cert verification (the native client pins by fingerprint, not CA);
- **flushes every frame** as it arrives, so server-streaming RPCs
  (`MediaPipeline.ResolveStream` and its `{progress}` pre-buffer narration) reach
  the browser incrementally instead of all at once when the RPC ends;
- CORS: `Allow-Origin: *`, and the preflight echoes back whatever the browser
  puts in `Access-Control-Request-Headers` (plus a static allow-list that
  includes `x-http-host` / `x-http-scheme`).

## The resolver and playable URLs

`MediaPipeline.ResolveStream` never hands the player an upstream URL (except
`direct_stream` plugins like Jellyfin). It returns a `/proxy/...` URL built as
`<scheme>://<host>/proxy/...`. Two things decide `<scheme>://<host>`:

1. **Host** — precedence, weakest to strongest:
   1. `127.0.0.1:<port>` (player on the same machine);
   2. `x-http-host` from client metadata — on the web path the bridge
      **synthesises** this from the request's `X-Forwarded-Host` (or `Host`);
      accepts a bare host, `host:port`, or a full origin; loopback is ignored;
   3. `peer.LocalAddr` of the native gRPC connection (always loopback on the web
      path, so it doesn't clobber #2);
   4. `server_host` setting / `MYCELIUM_SERVER_HOST` env — explicit operator
      override, may include a scheme (`https://media.example.com`).
2. **Scheme** — `x-http-scheme` metadata (bridge sets it from
   `X-Forwarded-Proto`, else `https` if mycelium itself terminates TLS, else
   `http`), or the scheme carried by a full-origin `x-http-host` / `server_host`.
   Native clients that send neither get `http` (unchanged behaviour).

**Why it matters:** a PWA served over `https://` cannot load `http://` media
(mixed content). The proxy URL must come back `https://…` and resolve to a host
the browser can reach.

## Operator checklist for the web path

- [ ] Terminate **TLS** in front of `/grpc`, `/proxy` and `/img` (reverse proxy
      or mycelium's own TLS), and have the reverse proxy send
      `X-Forwarded-Proto: https` and `X-Forwarded-Host`.
- [ ] If the reverse proxy does **not** set those headers, set the `server_host`
      setting (dashboard → *Host del server*) to the full public origin, e.g.
      `https://media.example.com` — otherwise proxy URLs come back
      `http://127.0.0.1:8000/...` and nothing plays.
- [ ] `network_mode: host` (or `go run` in development): `x-http-host` / `peer.LocalAddr` are
      enough; still needs TLS in front for an `https` PWA.
- [ ] Bridge / Docker-published ports: `server_host` is **mandatory** (neither
      the client hint nor the veth IP is the host-published address).
- [ ] `X-Forwarded-For` (used for per-IP rate limiting on `/admin/login`,
      `/proxy` and `/img`) is only trusted from a connection arriving via
      loopback by default. If your reverse proxy runs on a dedicated LAN IP
      instead of loopback, set `MYCELIUM_TRUSTED_PROXY_CIDRS` (comma-separated
      CIDRs, e.g. `MYCELIUM_TRUSTED_PROXY_CIDRS=10.0.0.5/32`) to that proxy's
      IP — otherwise its `X-Forwarded-For` is ignored and rate limiting keys
      on the proxy's own IP for every client behind it.
- [ ] HLS in Chrome/Firefox needs `hls.js` on the Dart side (Safari plays HLS
      natively). mycelium does not transmux.

## Known gaps

- The web app is served under `/app/`; `/` returns a JSON status blob. The Dart
  client uses an absolute `<origin>/grpc/` channel, so `/app/` is fine — moving
  the bundle to `/` would collide with existing root paths (`/health`,
  `/assets`, `/manifest.json`, …).
- `/proxy/*` URLs are HMAC-signed (`data`, `origin`, `cookies`, `xhdr`, `vpn`,
  `egr`, `sid`) — see `internal/core/proxy_sign.go` — so they can't be replayed
  or forged, but there's still no SSRF denylist on the resolved upstream host
  and no rate-limit. `/img` remains unsigned (fail-open image passthrough).
  Tracked for the pre-publication security pass — see the audit plan in the
  Obsidian vault.
