# mycelium

**mycelium** is a self-hosted media-server runtime written in Go. It provides the
infrastructure — HTTP APIs, an admin dashboard, an HLS proxy, client
authentication, a background scheduler — so you can focus on writing your own
**content providers** as isolated plugins.

It ships with **no content providers of its own**. What a mycelium instance can
browse and play is entirely defined by the plugins you install.

mycelium is the backend half of a two-part stack; a separate client app
([Pileus](#the-client)) consumes its APIs for playback.

### What mycelium is not

- **Not a content service.** It hosts, indexes and distributes no media, and
  neither the repository nor the Docker image contains any plugin.
- **Not a plugin store.** There is no built-in catalogue of plugins to
  download; the operator installs plugins they wrote or chose, and is
  responsible for what those plugins access.
- **Single-tenant.** One instance belongs to one household or operator, on
  their own hardware and network.

---

## How it works

The core knows nothing about any specific source. All content logic lives in
**plugins**: self-contained Lua packages you write and install. The core loads
them at startup, runs them in sandboxed VMs, schedules their background jobs,
and exposes their output through one unified API.

```
┌──────────────────────────────────────────────────────────────┐
│                        mycelium core                          │
│                                                              │
│  ┌───────────┐   ┌─────────────┐   ┌───────────────────────┐  │
│  │  Hub API  │   │  Admin API  │   │       HLS proxy       │  │
│  │ /api/v1/* │   │  /admin/*   │   │     /proxy/*      │  │
│  └─────┬─────┘   └──────┬──────┘   └───────────┬───────────┘  │
│        │                │                      │              │
│  ┌─────▼────────────────▼──────────────────────▼──────────┐   │
│  │        plugin engine (Lua VM pool, per plugin)         │   │
│  └───────┬───────────────┬───────────────┬────────────────┘   │
│          │               │      ┌────────▼─────────┐          │
│   ┌──────▼──────┐        │      │  background jobs  │          │
│   │  optional   │        │      │  (catalog sync,   │          │
│   │  browser    │        │      │   domain refresh) │          │
│   │  service    │        │      └──────────────────┘          │
│   │             │        │                                    │
│   └─────────────┘  ┌─────▼─────┐ ┌─────▼─────┐ ┌─────▼─────┐   │
│                    │ plugin A  │ │ plugin B  │ │ plugin C  │   │
│                    └───────────┘ └───────────┘ └───────────┘   │
└──────────────────────────────────────────────────────────────┘
```

A plugin runs in its own pool of Lua states; a fault in one plugin never takes
down the core or another plugin. Editing a plugin's files hot-reloads it.

---

## Getting started

### Requirements

- Go 1.26+
- (optional) Redis, for response caching — the core runs without it
- (optional) a browser service, if a plugin needs a real browser
  (see [Browser service](#browser-service))

### Run locally (development)

Docker (below) is the only supported way to run mycelium; `go run` is meant
for development.

```bash
git clone https://github.com/Lotho33/mycelium mycelium-core
cd mycelium-core
go run ./cmd/server
```

Open `http://localhost:8000/setup` to set the admin password, then install your
first plugin from the admin dashboard.

### Docker

The published multi-arch image (`linux/amd64`, `linux/arm64`) is the supported
way to run mycelium. The production stack (mycelium + Redis + Watchtower for
automatic updates) is described in
**[docs/deploy-selfhosted.md](docs/deploy-selfhosted.md)**:

```bash
git clone --depth 1 https://github.com/Lotho33/mycelium /opt/mycelium
cd /opt/mycelium
cp deploy/stack.env.example .env      # selects docker-compose.prod.yml
mkdir -p ~/.docker && [ -f ~/.docker/config.json ] || echo '{}' > ~/.docker/config.json
docker compose up -d
```

To build and run the image yourself:

```bash
docker build -t mycelium .
docker run -p 8000:8000 -p 50051:50051 \
  -v $(pwd)/plugins:/app/plugins \
  -v $(pwd)/data:/app/data \
  mycelium
```

`plugins/` and `data/` are mounted as volumes and persist across restarts.

The container starts as root only to take ownership of mycelium's own files in
those volumes (never other services' folders that may share `data/`), then
drops to an unprivileged user, uid:gid `10001` by default. Use
`MYCELIUM_PUID`/`MYCELIUM_PGID` to run as a different id (e.g. your own, so
bind-mounted files stay editable from the host), or `MYCELIUM_RUN_AS_ROOT=1` to
stay root.

### Configuration

Settings live in `data/config.json` and are editable from the admin dashboard
(HTTP port `8000` and gRPC port `50051` by default). A few can also be set via
environment variable:

| Variable                | Default                 | Description |
|-------------------------|-------------------------|-------------|
| `REDIS_ADDR`            | `localhost:6379`        | Redis address (cache; optional) |
| `MYCELIUM_SERVER_HOST`  | autodetected            | Address clients use for proxy/download URLs, e.g. `192.168.1.10:8000` or `https://media.example.com` |
| `MYCELIUM_TRUSTED_PROXY_CIDRS` | loopback         | Reverse proxies whose `X-Forwarded-*` headers are trusted |
| `MYCELIUM_STATIC_PAIRING_CODE` | —                | Reusable pairing code (≥ 10 chars) for a public demo instance; see [docs/deploy-selfhosted.md](docs/deploy-selfhosted.md) |
| `MYCELIUM_BROWSER_URL`  | —                       | Base URL of an optional browser service (see [Browser service](#browser-service)) |
| `MYCELIUM_BROWSER_API_KEY` | —                    | Sent as `X-Api-Key` to the browser service |
| `VPN_PROXY_URL`         | —                       | Proxy for plugin traffic, e.g. `socks5://proxy:1080` or `http://host:8888` |
| `MYCELIUM_HTTP_PROFILE` | —                       | `standard` forces direct upstream fetches even with a browser service configured |
| `MYCELIUM_DEBUG`        | —                       | `1` enables verbose logging |
| `MYCELIUM_PUID` / `MYCELIUM_PGID` | `10001`       | Docker: user/group the server drops to |

---

## Writing a plugin

Full reference: **[docs/plugin-sdk.md](docs/plugin-sdk.md)**. A quick tour:

A plugin is a directory under `plugins/<id>/` containing a `manifest.yaml`, an
`init.lua` entrypoint, and any number of sibling `.lua` modules (loaded via
`require`).

```
plugins/my_plugin/
├── manifest.yaml
├── init.lua
└── http.lua        # optional helper modules
```

### manifest.yaml

```yaml
id: my_plugin
name: My Plugin
version: 1.0.0
author: you
description: One line about what this plugin provides
icon: icon.svg

settings:
  global:
    - id: api_key
      label: "API key"
      type: password
      required: true

exposes:
  capabilities: [search, browse]
  catalogs:
    - id: popular
      name: Popular
      type: movie
      cache_ttl_seconds: 900

entrypoints:
  catalog: get_catalog
  search:  search_items
  details: get_details
  streams: get_streams
  resolve: resolve_stream
  browse:  browse

tasks:
  - function: sync_catalog
    cron: "@every 12h"
  - function: rebuild_index
    cron: ""          # empty cron = manual-only (a button in the dashboard)
```

| Section        | Purpose |
|----------------|---------|
| `settings`     | Fields rendered in the admin dashboard; values reach the plugin via `mycelium.context.get_global_setting`. |
| `exposes`      | Declared capabilities and the catalogs shown on the client home screen. |
| `entrypoints`  | Maps a pipeline step (`catalog`, `search`, `details`, `browse`, `streams`, `resolve`, …) to a Lua function name. |
| `tasks`        | Background jobs. `cron` is `@every <dur>`, a cron expression, or `""` (manual-only). `live_refresh: true` also lets the client trigger it on demand. `timeout_seconds` overrides the 30 s default. |

### init.lua

```lua
function get_catalog(args)
  local res = mycelium.network.get("https://api.example.com/popular?page=" .. args.page)
  if res.status_code ~= 200 then return nil, "upstream " .. res.status_code end
  local data = mycelium.json.parse(res.body)
  local items = {}
  for _, m in ipairs(data.results) do
    items[#items + 1] = { id = tostring(m.id), title = m.title, media_type = "movie", poster_url = m.poster }
  end
  return items
end

function resolve_stream(args)
  -- return the final playable URL for args.stream_id
  return { url = "https://cdn.example.com/" .. args.stream_id .. "/index.m3u8" }
end
```

### The `mycelium.*` SDK

Available inside every plugin:

| Module              | What it does |
|---------------------|--------------|
| `mycelium.network`  | `get` / `post` / `fetch` — HTTP with retries; routed through the proxy per the plugin's egress setting |
| `mycelium.dom`      | CSS-selector queries over an HTML string |
| `mycelium.json` / `mycelium.xml` | parse / stringify / path lookup |
| `mycelium.crypto`   | `base64_decode`, `aes_decrypt` (CBC) |
| `mycelium.cache`    | `set` / `get` / `del` — Redis-backed, namespaced per plugin |
| `mycelium.context`  | plugin settings, per-profile secrets, `set_plugin_status` |
| `mycelium.storage`  | `read_json` / `write_json` in the plugin's data area |
| `mycelium.image`    | server-side logo/luminance analysis |
| `mycelium.browser`  | `navigate` / `eval` / `sniff` via the optional browser service |
| `mycelium.progress` | narrate a long `resolve` to the client |
| `mycelium.log`, `mycelium.sleep` | logging into the plugin's log buffer; delay |

### Installing

Package the plugin directory as a ZIP and upload it from **Admin → Plugins →
Install**, or drop the directory into `plugins/` and restart. A plugin with
required settings unfilled stays *stopped* until you configure and start it.

---

## Browser service

Some sources need a real browser (to run JavaScript, or to observe the network
request that carries a stream URL). mycelium never launches a browser itself:
it talks over plain HTTP to an **optional browser service** you run separately
and point `MYCELIUM_BROWSER_URL` at. The service implements a small JSON API:

| Endpoint | Used by |
|----------|---------|
| `POST /v1/navigate` `{url, wait_for, timeout_ms, proxy_url}` → `{html, final_url}` | `mycelium.browser.navigate` |
| `POST /v1/eval` `{url, js, timeout_ms, proxy_url}` → `{result}` | `mycelium.browser.eval` |
| `POST /v1/sniff` `{trigger_url, url_pattern, timeout_ms, proxy_url}` → `{intercepted_url, headers}` | `mycelium.browser.sniff` |
| `POST /v1/fetch` `{url, headers, timeout_ms, proxy_url}` → upstream response, mirrored | HLS proxy upstream relay |
| `GET /health` → `{ready, engine}` | dashboard status |

If none is configured, `mycelium.browser.*` calls return "not available",
plugins that don't use them are unaffected, and the HLS proxy fetches
upstream directly. With one configured, the proxy relays its upstream fetches
through `/v1/fetch` (set `MYCELIUM_HTTP_PROFILE=standard` to keep them direct).
Every relayed target is checked against the same SSRF rules as a direct fetch
before it is handed over.

---

## Proxy / VPN routing

Set `VPN_PROXY_URL` to route plugin traffic (plugin HTTP calls, the browser
service, and the HLS relay) through an HTTP-CONNECT or SOCKS5 proxy. When a proxy is
configured the dialer is **fail-closed**: a request fails rather than falling
back to a direct connection. A plugin opts out with `direct_egress: true` in its
manifest — use it for a source on your own LAN (a personal media server) or one
that blocks the proxy's exit IP. Named egress profiles and per-plugin egress
selection are managed from the dashboard.

---

## API surface

- **Hub API** (`/api/v1/*`) — consumed by clients; every route requires an
  `X-Auth-Token` HMAC header generated per client from the dashboard.
- **Admin API** (`/admin/*`) — requires a session cookie from `POST /admin/login`.
- **HLS proxy** (`/proxy/*`) — rewrites playlists and relays segments/keys.
- **LAN discovery** — a client finds the server by broadcasting on UDP `:51900`.

---

## The client

mycelium is designed to be driven by **Pileus**, a separate client app (mobile,
desktop, TV and web) that talks to it over gRPC (port `50051` by default, or
gRPC-web under `/grpc/`). A device is paired once with a short-lived code
generated from the dashboard; profiles, PINs, watch progress and offline
downloads are kept on the server. The wire contract is documented in
[docs/pileus-contract.md](docs/pileus-contract.md), and the Pileus web build
can be served by mycelium itself under `/app/`.

---

## Project layout

```
mycelium-core/
├── cmd/server/        # entrypoint
├── internal/
│   ├── api/           # HTTP: admin, hub, HLS proxy, setup, discovery
│   ├── core/          # scheduler, HMAC/bcrypt, SSRF guard, shared HTTP client
│   ├── downloads/     # offline downloads (HLS → MKV via ffmpeg)
│   ├── engine/        # Lua plugin engine (manifest, VM pool, SDK, scheduler)
│   ├── managers/      # settings, SQLite stores, cache, egress
│   └── pileus/        # gRPC server for the client app
├── pkg/models/        # shared item types
├── third_party/       # vendored gRPC contract (proto + generated Go)
├── plugins/           # installed plugins (gitignored, one directory each)
├── web/               # admin dashboard (templates + static)
└── data/              # runtime state (gitignored)
```

---

## License

MIT — see [LICENSE](LICENSE).
