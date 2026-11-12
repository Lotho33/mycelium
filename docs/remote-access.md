# Remote access (design)

Status: **implemented** (phases 1–3: `internal/remote`, tailnet listeners,
admin API, dashboard card and overview warnings, setup notice,
`GetServerInfo` for native clients, Headscale wording). Not yet verified end
to end with a completed login: see [Implementation notes](#implementation-notes).

mycelium is LAN-only by design: the setup page, the discovery responder and
the default deployment all assume the server and its clients share a home
network. This document describes an **optional, opt-in** way to reach an
instance from outside the home: mycelium joins the user's own
[Tailscale](https://tailscale.com) tailnet (or a self-hosted
[Headscale](https://github.com/juanfont/headscale) one) **in-process**, through
the `tsnet` library, and gets a stable name with a valid HTTPS certificate.

---

## Problem

The typical operator:

- has **no DHCP reservation** (the ISP router doesn't offer one, or they don't
  know how), so the server's LAN IP changes;
- has **no domain and no reverse proxy**;
- uses Pileus on an **iPhone**, i.e. the PWA served at `/app/`. There is no
  native iOS build.

A PWA's identity is its **origin**: storage, the device token and the "Add to
Home Screen" icon are all bound to one URL. That URL must:

1. never change (no LAN IP in it);
2. work at home and away;
3. be `https://` with a certificate Safari trusts (secure context, and no
   mixed content for `/proxy/*` media, see [WEB.md](WEB.md)).

mDNS (`*.local`) fails 2 and 3. A LAN IP fails 1 and 2. The Tailscale IP
`100.x.y.z` fails 3. Running `tailscale serve` on the host works, but mycelium
can't see the result: the dashboard can't tell the user which URL to open, and
reading it from a host daemon (socket mounts, `tailscale status --json` from a
container) is fragile and not atomic.

## Goals

- **One URL per instance**, shown by the dashboard (text + QR), valid forever:
  `https://<name>.<tailnet>.ts.net`.
- **Set and forget**: one login from the dashboard, then no maintenance beyond
  what the dashboard warns about (key expiry, see below).
- **mycelium owns the state**: hostname, IPs, certificate readiness and login
  state are read in-process, never guessed.
- **No host changes**: no `tailscaled`, no TUN, no `cap_add`, no extra
  container. Same compose files.
- **Off by default.** An instance that never enables it behaves exactly as
  today, and doesn't load the code path.

## Non-goals

- **Public exposure.** Tailscale Funnel is deliberately **not** used: it would
  put an HLS proxy on the public Internet, relayed through Tailscale's servers
  with limited bandwidth. Video streaming doesn't fit there.
- **Replacing a reverse proxy.** Operators with a domain + nginx/Caddy keep
  `server_host` and their own TLS (see [WEB.md](WEB.md)); remote access is for
  everyone else.
- **Routing plugin egress through the tailnet.** `tsnet` here is only a
  listener. Outbound traffic (plugins, `/proxy` upstream fetches) is unchanged.
- **Being VPN-free on the client.** The client device must run the Tailscale
  app. See [Client side](#client-side) for why that's acceptable.

---

## User-facing flow

### Setup page

`/setup` gains a short, explicit notice (no action required there):

> **Mycelium è raggiungibile solo dalla rete di casa.**
> Per usarlo anche fuori casa, dopo la configurazione apri
> *Dashboard → Rete → Accesso da fuori casa*.

### Dashboard → Rete → "Accesso da fuori casa"

A new card on the existing `network` page, under "Uscite di rete" and
"Servizio browser". It shows one state at a time:

| State | What the card shows |
|---|---|
| `disabled` | One paragraph explaining what it does and what it needs (a free Tailscale account, the Tailscale app on each phone/TV that should work away from home). Button **Attiva**. |
| `starting` | Spinner, "Connessione a Tailscale…". |
| `needs_login` | "Accedi con il tuo account Tailscale per collegare questo server": the login link + QR (so it can be done from the phone). The page updates by itself once the login completes. |
| `needs_approval` | The tailnet requires device approval: "Approva il dispositivo *mycelium* nella console di Tailscale" + link to the admin console. |
| `running` | The URL `https://mycelium.tailXXXX.ts.net/app/` as text, copy button and **QR**. Below it: certificate status, node key expiry, buttons **Disconnetti** / **Disattiva**. |
| `running`, no HTTPS | The URL is reachable but HTTPS isn't enabled in the tailnet: "Abilita *HTTPS Certificates* nella console di Tailscale (DNS → HTTPS)" + link. The QR is hidden until the certificate is ready, so nobody installs an `http://` PWA. |
| `error` | The last error, in plain words, + **Riprova**. |

The `running` card also shows the step-by-step instructions for the phone:

1. Installa l'app Tailscale e accedi con lo **stesso account**.
2. Lascia Tailscale attivo (in casa la connessione resta diretta, non passa da Internet).
3. Inquadra il QR, apri il link in Safari → Condividi → *Aggiungi alla schermata Home*.
4. Abbina il dispositivo con un codice (*Abbina un dispositivo*), come in casa.

---

## Architecture

```
                 ┌─────────────────────────── mycelium process ───────────────────────────┐
                 │                                                                        │
 LAN ──────────▶ │  net.Listen :8000 (HTTP)  ─┐                                           │
 LAN ──────────▶ │  net.Listen :50051 (gRPC) ─┼──▶ same mux / same grpc.Server            │
                 │                            │                                           │
 tailnet ──────▶ │  tsnet  :443 (TLS, LE)  ───┤   internal/remote                         │
 tailnet ──────▶ │  tsnet  :50051 (gRPC)   ───┘     ├─ tsnet.Server (userspace WireGuard)  │
                 │                                  ├─ state machine + status snapshot     │
                 │                                  └─ admin endpoints + SSE topic         │
                 └────────────────────────────────────────────────────────────────────────┘
```

### New package: `internal/remote`

Owns the `tsnet.Server` and exposes a narrow interface to the rest of the
code:

```go
type State string // disabled | starting | needs_login | needs_approval | running | error

type Status struct {
    State       State
    AuthURL     string     // needs_login only
    FQDN        string     // "mycelium.tail1234.ts.net", no trailing dot
    IPv4, IPv6  netip.Addr
    HTTPSReady  bool       // certificate issued for FQDN
    CertDomains []string   // empty ⇒ HTTPS not enabled in the tailnet
    KeyExpiry   *time.Time // nil when expiry is disabled for the node
    ControlURL  string     // "" = Tailscale's default
    Err         string
}

type Manager interface {
    Enable(ctx context.Context) error // persist + start
    Disable() error                   // stop listeners, keep node state
    Logout(ctx context.Context) error // log the node out, wipe state dir
    Status() Status                   // cheap snapshot, never blocks
    Subscribe() (<-chan Status, func())
}
```

Everything else (dashboard, `/pileus/info`, the resolver in
`internal/pileus/media_handler.go`) reads `Status()`. Nobody else imports
`tailscale.com`, so the dependency stays contained and the interface can be
faked in tests.

### `tsnet` lifecycle

- `tsnet.Server{Dir: data/tailscale, Hostname: remote_hostname, ControlURL:
  remote_control_url, AuthKey: env}`. `Ephemeral: false`: the node must
  survive restarts with the same name.
- `Logf` → discarded (very chatty). `UserLogf` → mycelium's log with a
  `[remote]` prefix.
- **`Start()`, not `Up()`**: `Up` blocks until the node is running, which
  never happens while it waits for an interactive login. After `Start`, the
  manager watches the IPN bus (`LocalClient().WatchIPNBus(ctx,
  ipn.NotifyInitialState|ipn.NotifyInitialNetMap)`) and maps each notification
  to a `State`:
  - `BrowseToURL` / backend `NeedsLogin` → `needs_login` + `AuthURL`;
  - `NeedsMachineAuth` → `needs_approval`;
  - `Running` → read `Status` (`Self.DNSName`, `Self.TailscaleIPs`,
    `Self.KeyExpiry`, `CertDomains`) → `running`.
  In recent Tailscale releases the local client lives in
  `tailscale.com/client/local`. Pin the exact API during the prototype.
- **The hostname is read, never assumed.** If `mycelium` is taken in the
  tailnet, the control server assigns `mycelium-1`. The FQDN always comes from
  `Self.DNSName`.
- **Certificate pre-warm**: once `running` with a non-empty `CertDomains`,
  fetch the certificate in the background (`LocalClient().CertPair`) so the
  first handshake from the phone doesn't wait on ACME. `HTTPSReady` flips
  when it succeeds. Renewal is handled by `tsnet`.
- Startup at boot only when `remote_access_enabled` is true, **in the
  background** (`core.SafeGo`), after the LAN listeners are up: a slow or
  unreachable control server must never delay serving the LAN.

### Listeners on the tailnet

| tsnet listener | Serves | TLS |
|---|---|---|
| `ListenTLS("tcp", ":443")` | the same `http.Handler` as `:8000` (dashboard, `/app/`, gRPC-web, `/proxy`, `/img`) | Let's Encrypt cert for the FQDN, managed by `tsnet` |
| `Listen("tcp", ":80")` | redirect only → `https://<fqdn>` | none |
| `Listen("tcp", ":"+pileus_grpc_port)` | the existing `grpc.Server` (native clients) | the existing self-signed cert, pinned by fingerprint as today |

Consequences, checked against the current code:

- `r.TLS != nil` on the `:443` listener, so the gRPC-web bridge
  (`internal/pileus/grpcweb.go`) already sets `x-http-scheme: https` and
  `x-http-host` from `Host`. `/proxy/*` URLs come back as
  `https://<fqdn>/proxy/…` with **no `server_host` needed**. Mixed content is
  solved by construction.
- The same `http.Server` settings (timeouts, header limits, middleware chain)
  apply. Build the handler once in `main.go` and serve it from both
  listeners.
- `realIP` sees the peer's `100.x` address. Connections aren't from loopback,
  so `X-Forwarded-For` stays untrusted: correct.
- **Shutdown**: close the tsnet listeners together with `srv.Shutdown` and
  `pileusSrv.GracefulStop`, then `tsnet.Server.Close()`.

### Settings and environment

| Setting (dashboard) | Env override | Default | Meaning |
|---|---|---|---|
| `remote_access_enabled` | — | `false` | Start the node at boot |
| `remote_hostname` | `MYCELIUM_REMOTE_HOSTNAME` | `mycelium` | Requested machine name |
| `remote_control_url` | `MYCELIUM_REMOTE_CONTROL_URL` | `""` | Headscale URL; empty = Tailscale |
| — | `MYCELIUM_REMOTE_AUTHKEY` (or `TS_AUTHKEY`) | — | Headless login with a pre-auth key, skips `needs_login` |

The node state lives in `data/tailscale/` (mode `0700`, already inside the
data volume). The settings go through the allowlist of `Settings.Save`.

### Admin API

All under `/admin/`, so they inherit the admin session and the CSRF guard:

- `GET  /admin/remote/status` → `Status` as JSON (without internal errors'
  stack, without the auth key);
- `POST /admin/remote/enable`, `POST /admin/remote/disable`,
  `POST /admin/remote/logout`;
- `PUT  /admin/remote/settings` → hostname / control URL (applied with a
  restart of the node, not of the process);
- an SSE topic `remote` on the existing admin events stream
  (`internal/api/admin_events.go`), so the card leaves `needs_login` by
  itself the moment the login completes on the phone.

The QR is rendered server-side as SVG (`GET /admin/remote/qr.svg?for=app|login`,
`rsc.io/qr`): no JS library, no external CDN, and it encodes only the app URL
or the login link, never arbitrary text.

---

## Client side

### iPhone (PWA)

Nothing to change in Pileus: on the web path the gRPC host is the page's own
origin (`host_resolver.dart`, `_webHost`). The tailnet URL is a **different
origin** from `http://192.168.x.x:8000/app/`, so it's a separate install with
its own pairing. The card says to use **only** the tailnet URL on the iPhone,
at home too.

Why "Tailscale always on" is acceptable, and what the docs must say:

- it's a **split tunnel**: only tailnet traffic enters it, the rest of the
  phone is unaffected;
- at home the peers find each other on the LAN and talk **directly**. The
  relays (DERP) are only used when NAT traversal fails;
- idle cost is negligible;
- **iOS allows one active VPN at a time**: a user who already runs a
  corporate or commercial VPN will hit a conflict. State it plainly.

### Native clients (Android, TV, desktop)

Already half there: the native resolver races UDP discovery against a list
of candidates that includes `DeviceSession.vpnHost`. Today that value has to
be typed by hand. Proposal:

- mycelium publishes `{"remote_host": "<fqdn>", "remote_grpc_port": N}` to
  **paired devices only** (e.g. a field in an authenticated `AuthService`
  response, refreshed at each connect), **not** in the unauthenticated
  `/pileus/info`, so the tailnet name isn't handed to anyone on the LAN who asks;
- Pileus stores it as `vpnHost`. At home UDP discovery wins (LAN IP, no VPN
  needed). Away, if the device runs Tailscale, `vpnHost` wins.

This is a coordinated change with pileus-player; record it in
[pileus-contract.md](pileus-contract.md) when it lands.

---

## Security

- **Who can reach it**: every device of the tailnet. For a single-person
  tailnet that's the owner's own devices. A shared or multi-user tailnet
  should restrict it with ACLs / tags. The docs link Tailscale's ACL docs and
  say so. The mycelium login and device pairing remain the real gate. Remote
  access doesn't relax any of them.
- **`setupAllowedFrom` already treats `100.64.0.0/10` as local**, so an
  instance not yet set up is configurable from the tailnet. That's acceptable
  (remote access can only be enabled *after* setup, by an admin), but keep it
  in mind if the CGNAT rule ever changes.
- **Funnel is never enabled**: the manager doesn't call `ListenFunnel`, and a
  test asserts it.
- **Certificate Transparency**: the FQDN (`mycelium.tailXXXX.ts.net`) becomes
  public in CT logs once the certificate is issued. It reveals the name, not
  reachability. The card mentions it next to the HTTPS step.
- **Secrets**: the auth key is read from env only, never persisted, never
  returned by `/admin/remote/status`, redacted from logs.

### Node key expiry ("set and forget" killer)

Tailscale expires the keys of user-owned nodes periodically (180 days by
default). When that happens remote access stops **silently** and the phone
shows a connection error. Mitigations:

- the card shows `KeyExpiry` and a button to the console page where expiry
  can be disabled for this machine (recommended for a server);
- from 14 days before expiry, a dashboard banner (same mechanism as "update
  available");
- nodes logged in with a **tagged** auth key have expiry disabled by default:
  mention it as the headless option.

---

## A Tailscale already on the machine

Some installs run Tailscale next to mycelium (a `tailscale/tailscale`
container with host network, or the host's own `tailscaled`). mycelium
detects it on its own: an interface named `tailscale*`, or one holding a
`fd7a:115c:a1e0::/48` address (100.64/10 alone isn't enough, ISPs use it for
CGNAT). tsnet creates no interface, so mycelium's own node is never mistaken
for it. Detection is cached for 15 s.

**While it runs, mycelium uses it**: no node of its own, no extra memory.
With host networking mycelium already answers on that node's addresses, so:

- the card shows state `external`, with the node's address;
- `GetServerInfo` hands paired devices that address, so the native apps
  connect away from home by themselves;
- the App Pileus page shows `http://<name>:<http port>/app/` for iPhone, iPad
  and Mac. The name is the node's MagicDNS name, asked in the background to
  the tailnet resolver (`100.100.100.100`, PTR); without MagicDNS, the
  Tailscale IPv4, stable for the node's lifetime;
- clients that reach the server through it get proxy URLs on that address,
  `server_host` notwithstanding.

What it can't give is an HTTPS certificate: that takes the daemon's local API,
which the container doesn't share with mycelium. The web app is served over
http, as it is at home, inside the tailnet's WireGuard encryption.

**Moving to mycelium's own node** (for HTTPS, or to drop the container). Two
daemons must never be the same node, so with the feature turned on mycelium's
node waits (`host_tailscale`, rechecked every 15 s) while the other one runs,
and starts by itself within ~30 s of it going away. If that container kept
its state in mycelium's `data/tailscale` (a `./data/tailscale:/var/lib/tailscale`
mount next to `./data:/app/data`), the node takes it over: tsnet reads the
same `tailscaled.state` format, so it is the same machine, with the same name,
the same `100.x` address and no new login. The container wrote those files as
root: the entrypoint gives `data/tailscale` to the mycelium user at every
start, so mycelium must have been restarted (an image update does it) after
the files were written.

Steps, with remote access over that same tailnet working throughout except
for a few seconds:

1. In the dashboard, *Nome e server*: set the machine name the container used
   (`TS_HOSTNAME`), or Tailscale renames the node to the new one.
2. Turn the feature on ("Use mycelium's own node when possible"): the card
   keeps showing the external node until it goes away.
3. On the machine: `docker compose stop tailscale onboarding`, then remove
   both services from the compose file (otherwise the next `up -d` brings
   them back). Within ~30 s mycelium's node is up under the old address.

State kept elsewhere (e.g. `./tailscale`): copy it first, with the container
stopped: `cp -a tailscale/. data/tailscale/ && chown -R 10001:10001
data/tailscale`, then restart mycelium. Without any old state the node asks
for a new login like a fresh install.

## Backup, restore and moving machines

`data/tailscale/` holds the node's private key. It is **excluded** from the
`.mycelium` backup: restoring it on a second machine while the first is still
running would make two processes claim the same node. After an import, the
card is in `needs_login` and one login re-creates the node. The old machine
entry can be removed from the Tailscale console. `remote_access_enabled`,
`remote_hostname` and `remote_control_url` *are* exported like any other
setting.

---

## Dependency, size, resources

- **Licence**: `tailscale.com` is BSD-3-Clause, compatible with mycelium's
  MIT licence. Its notice must ship with the binary and the image: add a
  generated `THIRD_PARTY_NOTICES` (e.g. `go-licenses report`) to the release
  workflow and copy it into the image. That's worth doing for all dependencies
  anyway.
- **Naming**: refer to it descriptively ("accesso remoto tramite Tailscale",
  "works with Tailscale"). No Tailscale logo in the UI, nothing implying
  endorsement or partnership. Check Tailscale's trademark guidelines once
  before the first release that ships it.
- **Binary size**: `tsnet` pulls in a large dependency tree. Measure the
  delta on `linux/arm64` in the prototype; if it's unacceptable, gate it behind
  a build tag (`-tags remote`) and ship it in the default image only.
- **Memory**: low-RAM hosts run with a 350 MiB soft limit (`main.go`). Measure
  the RSS of an idle `running` node. The node isn't started at all when the
  feature is disabled.
- **DNS**: `main.go` replaces `net.DefaultResolver` with public resolvers
  (1.1.1.1 / 8.8.8.8). Fine for Tailscale's control plane. A Headscale
  `ControlURL` must therefore be **publicly resolvable** (or an IP). A LAN-only
  hostname won't resolve. Verify in the prototype which resolver `tsnet`'s
  control client actually uses, and document the result.
- **Upkeep**: keep `tailscale.com` current (Renovate/Dependabot on `go.mod`).
  Very old clients are eventually refused by the control server.
- **Networking**: no changes to the compose files. With `network_mode: host`
  direct connections work through NAT traversal without port forwarding.

### Headscale

`ControlURL` makes the whole feature work against Headscale for the
connectivity part. HTTPS certificates depend on the control server supporting
the cert flow. Verify what current Headscale supports. If it doesn't, the card
shows `running`, no HTTPS with a Headscale-specific explanation: the native
clients work (pinned gRPC TLS), the iPhone PWA doesn't.

---

## Implementation notes

Measured and decided while building it (11/2026):

- **Version**: `tailscale.com v1.104.6`, which needs Go ≥ 1.27.3: the project
  moved to Go 1.27.4 (go.mod, release `Dockerfile`, devcontainer) and CI to
  staticcheck 2026.2.2 / govulncheck v1.9.0 (the old pins can't read Go 1.27
  export data).
- **Size and memory** (`linux/amd64`, `-trimpath -ldflags="-s -w"`): the
  binary grows from 32.3 MB to 50.9 MB (+18.6 MB). Idle RSS with the feature
  **off** goes from 26.5 MB to 35.7 MB (+9 MB: text pages and package
  init). A node waiting for login uses about 32 MB on its own. `-tags
  noremote` builds a stub without `tailscale.com` (`internal/remote/stub.go`);
  the default build and the image include it.
- **Log upload off**: tsnet uploads its logs to Tailscale's log service by
  default. The manager sets `TS_NO_LOGS_NO_SUPPORT` before the first start.
  tsnet still keeps a local log buffer in `data/tailscale/`.
- **Extra tailnet listener on the LAN HTTP port** (in clear; WireGuard already
  encrypts it). Native clients mint `/proxy` URLs from the address they
  dialed, `<tailscale ip>:<http port>`, so that port must answer on the
  tailnet too. `:80` serves the page until the certificate is ready, then
  redirects requests for the FQDN to `https://`.
- **`:443` uses tsnet's `Listen` + `LocalClient().GetCertificate`**, not
  `ListenTLS`: `ListenTLS` blocks until the node is running, which never
  happens while it waits for an interactive login.
- **`server_host` vs the tailnet**: a client that reached the server by its
  tailnet name or address (the `x-http-host` hint or the gRPC local address)
  gets proxy URLs on that name/address, and `server_host` is skipped for it.
  A bare tailnet name (`https://<fqdn>/`) means the scheme's default port,
  not `:8000`. LAN clients are unchanged.
- **`http.DefaultTransport`** (replaced in `main.go` for the public DNS
  resolvers) now has a non-nil, empty `TLSClientConfig`: tailscale's control
  client clones the default transport and edits its TLS config, and panicked
  on nil. HTTP/2 to upstreams stays off as before.
- **Settings** (`remote_access_enabled`, `remote_hostname`,
  `remote_control_url`) are written by the `/admin/remote/*` endpoints
  (validated), not through the generic settings allowlist. Saving the name or
  control URL restarts the node, not the process.
- **Logout** ("Scollega account") logs the node out (best effort), wipes
  `data/tailscale/` and, while the feature stays on, starts a fresh node that
  asks for a new login.
- **Live updates**: the card follows the `remote` topic of the existing
  `/admin/events` stream (snapshot-based, like the other sections).
- **Native clients** (phase 2): `AuthService.GetServerInfo` returns the
  Tailscale **IPv4** as `remote_host` (works with MagicDNS off; the FQDN is
  in `remote_fqdn`), plus the gRPC and HTTP ports. A dedicated RPC rather
  than a field of `AuthorizeDeviceResponse`: `RefreshToken` only runs in the
  last week before the JWT expires, too rarely to pick up the address.
  Pileus calls it at every start and after pairing; see
  [pileus-contract.md](pileus-contract.md).
- **Warnings** (phase 3): the dashboard overview shows a warning when the
  feature is on but the node isn't connected (expired key, login never
  completed, error) and from 14 days before the key expires. Headscale gets
  its own wording for approval (`headscale nodes register`) and key expiry.
- **Licences**: `scripts/third-party-notices.sh` builds
  `/app/THIRD_PARTY_NOTICES` in the image from the modules actually linked
  into `server` and `wireproxy` (`go version -m`), plus the Go licence;
  FFmpeg's licence (LGPL 2.1+ as built) and source location go to
  `/app/licenses/ffmpeg/`.
- **Tested**: unit tests (state mapping, snapshot, hostname validation,
  tailnet-host matching, redirect, no Funnel), handler tests with a fake
  manager, proxy-URL tests; end to end in the devcontainer against the real
  Tailscale control server up to `needs_login` with a real login link and
  QR, restart (node back at boot), logout, disable, CSRF, with both
  tailscale 1.102.5 and 1.104.6. Not yet tested: a completed login,
  certificate issuance, the iPhone PWA and a native client over the tailnet.

## Documentation changes (when implemented)

- **README**: a short "Accesso da fuori casa" section: LAN-only by default,
  optional remote access through Tailscale/Headscale, link to this doc's
  user-facing part.
- **deploy-selfhosted.md** → *Networking*: one paragraph, "no compose change
  needed, state in `data/tailscale/`".
- **WEB.md** → *Operator checklist*: the tailnet path needs none of the TLS /
  `server_host` steps.
- **pileus-contract.md**: the `remote_host` field for native clients.

---

## Rollout

1. **Core**: `internal/remote`, tailnet listeners, admin API, dashboard card,
   setup notice. iPhone PWA works end to end.
2. **Native clients**: `remote_host` to paired devices + Pileus storing it as
   `vpnHost`.
3. **Polish**: key-expiry banner, Headscale messaging, size/memory
   measurements written back into this doc.

## Testing

- `internal/remote` behind the `Manager` interface: unit-test the
  IPN-notification → `State` mapping with recorded notifications, and the
  admin endpoints and card states with a fake manager.
- `main.go` wiring: a test that the tailnet handler is the same handler as the
  LAN one (middleware chain included) and that no Funnel listener is created.
- One manual end-to-end checklist per release that touches it: fresh login
  from the dashboard, iPhone PWA install over the tailnet URL, playback of an
  HLS stream (proxy URLs come back `https://<fqdn>`), restart keeps the node,
  logout wipes it.

## Open questions

- Hostname default: `mycelium` or something per-instance (`mycelium-<short
  id>`) to avoid `-1` suffixes for people running two instances?
- Should the dashboard itself be reachable over the tailnet, or only `/app/`,
  gRPC and the media endpoints? (Reachable is simpler and matches the
  LAN; a toggle could come later.)
- `remote_host` delivery to native clients: which authenticated RPC carries
  it.
