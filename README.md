# mycelium

**mycelium** is a self-hosted media-server runtime written in Go. It provides the
infrastructure — HTTP APIs, an admin dashboard, an HLS proxy, client
authentication, a background scheduler — so you can focus on writing your own
**content providers** as isolated plugins.

It ships with **no content providers of its own**. What a mycelium instance can
browse and play is entirely defined by the plugins you install.

mycelium is the backend half of a two-part stack; a separate client app
([Pileus](#the-client)) consumes its APIs for playback.

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
│  │ /api/v1/* │   │  /admin/*   │   │     /api/proxy/*      │  │
│  └─────┬─────┘   └──────┬──────┘   └───────────┬───────────┘  │
│        │                │                      │              │
│  ┌─────▼────────────────▼──────────────────────▼──────────┐   │
│  │        plugin engine (Lua VM pool, per plugin)         │   │
│  └───────┬───────────────┬───────────────┬────────────────┘   │
│          │               │      ┌────────▼─────────┐          │
│   ┌──────▼──────┐        │      │  background jobs  │          │
│   │  companion  │        │      │  (catalog sync,   │          │
│   │  browser    │        │      │   domain refresh) │          │
│   │  service    │        │      └──────────────────┘          │
│   │ (optional)  │        │                                    │
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
- (optional) a companion browser service, if a plugin needs a real browser
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

```bash
docker build -t mycelium .
docker run -p 8000:8000 \
  -v $(pwd)/plugins:/app/plugins \
  -v $(pwd)/data:/app/data \
  mycelium
```

`plugins/` and `data/` are mounted as volumes and persist across restarts.

### Configuration

Settings live in `data/config.json` and are editable from the admin dashboard.
A few can also be set via environment variable:

| Variable                | Default                 | Description |
|-------------------------|-------------------------|-------------|
| `SERVER_PORT`           | `8000`                  | HTTP listen port |
| `REDIS_ADDR`            | `localhost:6379`        | Redis address (cache; optional) |
| `COBWEB_ADDR`           | `http://localhost:8191` | Base URL of the companion browser service (optional) |
| `VPN_PROXY_URL`         | —                       | Proxy for plugin traffic, e.g. `socks5://proxy:1080` or `http://host:8888` |
| `MYCELIUM_HTTP_PROFILE` | `standard`              | `standard` (net/http) or `browser` (see [HTTP profile](#http-profile)) |
| `MYCELIUM_DEBUG`        | —                       | `1` enables verbose logging |

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
| `mycelium.browser`  | `navigate` / `eval` / `sniff` via the companion browser service |
| `mycelium.progress` | narrate a long `resolve` to the client |
| `mycelium.log`, `mycelium.sleep` | logging into the plugin's log buffer; delay |

### Installing

Package the plugin directory as a ZIP and upload it from **Admin → Plugins →
Install**, or drop the directory into `plugins/` and restart. A plugin with
required settings unfilled stays *stopped* until you configure and start it.

---

## Browser service

Some sources need a real browser (to run JavaScript, or to observe the network
request that carries a stream URL). mycelium never launches a browser itself; it
talks over plain HTTP to an **optional companion browser service** —
[cobweb](https://github.com/Lotho33/cobweb), a separate project — that you run
separately and point `COBWEB_ADDR` at. Each `mycelium.browser.*` call from a
plugin becomes one HTTP request to that service. If none is configured,
`mycelium.browser.*` calls simply return "not available" and plugins that don't
use them are unaffected.

When an upstream asks for an interactive verification (a CAPTCHA) that the
service can't complete on its own, the dashboard surfaces it under
**Verifiche in sospeso** so the operator can complete it once in a real browser
session; the resulting cookies are reused for later automated calls.

---

## HTTP profile

`MYCELIUM_HTTP_PROFILE` (or the `http_profile` setting) controls how the HLS
proxy's upstream fetches present themselves:

- `standard` (default) — Go's `net/http` stack.
- `browser` — a mainstream-browser TLS ClientHello + HTTP/2 profile. Go's own
  stack has a distinct TLS/HTTP2 fingerprint that some CDNs refuse regardless of
  request headers; a plugin talking to such a CDN can opt in here.

---

## Proxy / VPN routing

Set `VPN_PROXY_URL` to route plugin traffic (scraping, the browser service, and
the HLS relay) through an HTTP-CONNECT or SOCKS5 proxy. When a proxy is
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
- **HLS proxy** (`/api/proxy/*`) — rewrites playlists and relays segments/keys.
- **LAN discovery** — a client finds the server by broadcasting on UDP `:51900`.

---

## The client

mycelium is designed to be driven by a dedicated client app over its gRPC API
(port `50051` by default). The client is a separate project and not required to
run or develop mycelium — any client that speaks the Hub API works.

---

## Project layout

```
mycelium-core/
├── cmd/server/        # entrypoint
├── internal/
│   ├── api/           # HTTP: admin, hub, HLS proxy, setup, discovery
│   ├── core/          # scheduler, HMAC/bcrypt, shared HTTP client
│   ├── engine/        # Lua plugin engine (manifest, VM pool, SDK, scheduler)
│   ├── managers/      # settings, SQLite client store, cache, egress
│   └── pileus/         # gRPC server for the client app
├── plugins/           # installed plugins (one directory each)
├── web/               # admin dashboard (templates + static)
└── data/              # runtime state (gitignored)
```

---

## License

MIT — see [LICENSE](LICENSE).
