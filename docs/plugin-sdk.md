# Writing a mycelium plugin

A plugin teaches mycelium about one content source. It is a directory of Lua
files plus a manifest; mycelium loads it into a sandboxed VM pool, schedules its
background jobs, and routes client requests to it through one unified pipeline.

There is no build step and no toolchain: drop the directory in `plugins/`, or
upload a ZIP from the admin dashboard. Editing a file hot-reloads the plugin.

---

## Layout

```
plugins/my_plugin/
├── manifest.yaml     # required — metadata, settings, entrypoints, tasks
├── init.lua          # required — defines the entrypoint / task functions
├── icon.svg          # optional — shown in the client side menu
└── *.lua             # optional — helper modules, loaded via require("name")
```

Every `*.lua` file in the directory (except `init.lua`) is available as
`require("<basename>")`. `init.lua` is executed at load time; it should
`require` its helpers and define the functions named in the manifest.

---

## manifest.yaml

```yaml
id: my_plugin                 # unique; also the on-disk directory name
name: My Plugin               # shown to users
version: 1.0.0
author: you
description: One line about what this plugin provides
icon: icon.svg                # path relative to the plugin dir

pool_size: 2                  # concurrent Lua VMs for this plugin (default 2)

# --- network routing (all optional) ---
direct_egress: true           # opt OUT of the default proxy/VPN routing
vpn_optional: true            # (with direct_egress) add an admin toggle that
                              # routes only the video flow through the proxy
direct_stream: true           # hand the resolved URL to the player as-is,
                              # skipping mycelium's HLS proxy (LAN sources only)

settings:
  global:
    - id: api_key
      label: "API key"
      type: password           # string | password | number | bool
      hidden: true             # password fields: don't echo the value back
      required: true           # plugin stays "waiting" until every required
                               # setting has a value

exposes:
  capabilities: [search, browse]      # hints for the client UI
  catalogs:                            # rows on the client home screen
    - id: popular
      name: Popular
      type: movie                      # movie | series | live | music | audiobook | vod_clip
      cache_ttl_seconds: 900
      auto_hide_when_empty: true       # hide the row when page 1 came back empty
      section_kind: carousel           # carousel | grid  (client hint)
      style_hint: featured             # free-form hint within section_kind
      card_layout: landscape           # "" (poster 2:3) | landscape (16:9)
      disable_hero_background: true     # never use this row's art in the hero

entrypoints:                    # pipeline step  ->  Lua function name
  catalog:        get_catalog
  catalog_list:   get_catalog_list     # optional — fully dynamic catalog list
  search:         search_items
  search_filters: get_search_filters   # optional
  details:        get_details
  browse:         browse
  streams:        get_streams
  resolve:        resolve_stream

tasks:
  - function: sync_catalog
    cron: "@every 12h"          # "@every <dur>" | cron expression | "" (manual)
  - function: refresh_now
    cron: "@every 2m"
    live_refresh: true          # client can also trigger it on demand
  - function: rebuild_index
    cron: ""                    # manual-only: a button in the dashboard
    timeout_seconds: 600        # overrides the 30 s default for this task
```

`cron: ""` means the task never runs on its own — it only appears in the
dashboard with a "Run" button. `timeout_seconds` is capped at 15 minutes.

---

## Entrypoints

Each entrypoint is a global Lua function `f(args)` that returns
`value` or `value, "error string"`. `args` is a table; the fields below are what
mycelium passes.

| Entrypoint       | `args` fields                        | returns |
|------------------|--------------------------------------|---------|
| `get_catalog`    | `catalog_id`, `page` (1-based)       | array of items, or `{ items = [...], has_more = bool }` |
| `get_catalog_list` | —                                  | array of catalog defs (same shape as `exposes.catalogs`) |
| `search_items`   | `query`, `page`, `filters` (map)     | array of items |
| `get_search_filters` | —                                | array of filter defs (see [search-filters.md](search-filters.md)) |
| `get_details`    | `media_id`                          | one item with full metadata |
| `browse`         | `directory_id`, `page`              | array of items; use `is_dir = true` for folders |
| `get_streams`    | `media_id`                          | array of stream sources: `{ id, label, quality, is_live }` |
| `resolve_stream` | `stream_id`, `force_refresh` (bool) | `{ url, headers, is_live, extra }` |

### Item shape

```lua
{
  id          = "movie:123",     -- opaque, plugin-defined
  title       = "…",
  media_type  = "movie",         -- movie | series | episode | live | ...
  poster_url  = "https://…",
  year        = 2021,
  plot        = "…",
  is_dir      = false,           -- true → a browsable folder
  -- episodes/series: parent_id, season_number, episode_number, …
  extra       = { fanart_url = "https://…" },
}
```

### `resolve_stream` result

```lua
{
  url     = "https://cdn.example/index.m3u8",  -- final playable URL
  headers = { Referer = "…", Cookie = "…" },   -- replayed by the HLS proxy
  is_live = false,
  extra   = { … },                             -- plugin-specific passthrough
}
```

`force_refresh` is set by mycelium when a previously-resolved URL was rejected
downstream — skip any cache and resolve fresh.

---

## The `mycelium.*` SDK

Available as globals inside every plugin function.

### `mycelium.network`

```lua
local res = mycelium.network.get(url [, headers_table [, timeout_seconds]])
local res = mycelium.network.post(url, body [, headers_table [, timeout_seconds]])
-- res = { status_code = 200, body = "…", headers = { ["Set-Cookie"] = "a\nb" }, error = nil }

local body, err = mycelium.network.fetch(url [, { method=, body=, headers=, timeout_seconds= }])
```

Transport-level failures are retried a few times. Requests go through the
plugin's configured egress (proxy/VPN) unless the plugin is `direct_egress`. A
`direct_egress` plugin can set the header `_proxy = true` on a single request as
a best-effort opt-in.

### `mycelium.browser`

Reaches a real browser via the optional companion browser service (`COBWEB_ADDR`).
Returns `"browser service not available"` when none is configured.

```lua
local html, final_url, err = mycelium.browser.navigate(url [, timeout_seconds])
local result, err          = mycelium.browser.eval(url, js_code [, timeout_seconds])
local hit_url, headers, err = mycelium.browser.sniff(trigger_url, url_pattern [, timeout_seconds])
```

`sniff` opens `trigger_url` and returns the first network request whose URL
matches `url_pattern` (glob), plus that request's headers — the usual way to
grab a stream URL a page builds in JavaScript.

### `mycelium.dom`

```lua
mycelium.dom.query(html, css)        -- text of the first match
mycelium.dom.query_all(html, css)    -- array of texts
mycelium.dom.attr(html, css, name)   -- attribute of the first match
mycelium.dom.select(html, css)       -- array of { text, html, attr = fn }
```

### `mycelium.json` / `mycelium.xml`

```lua
local t, err = mycelium.json.parse(str)
local str    = mycelium.json.stringify(value)
local v      = mycelium.json.get(str, "path.to[0].field")   -- gjson syntax
local arr    = mycelium.json.get_array(str, "path")
local node, err = mycelium.xml.parse(str)   -- { _tag, _text, _attrs, [1..n] children }
```

### `mycelium.crypto`

```lua
local s, err = mycelium.crypto.base64_decode(b64)                 -- std / url / raw
local s, err = mycelium.crypto.aes_decrypt(ciphertext_b64, key_hex, iv_hex)  -- AES-CBC 128/256
```

### `mycelium.cache`

Redis-backed, namespaced per plugin. No-ops (with a `false` return) when Redis
is absent.

```lua
local ok, err  = mycelium.cache.set(key, value, ttl_seconds)
local val, hit = mycelium.cache.get(key)
mycelium.cache.del(key)
```

### `mycelium.context`

```lua
mycelium.context.get_global_setting("api_key")        -- an admin-configured setting
mycelium.context.get_secret(key)                     -- per-profile, stored in SQLite ("" if unset)
local ok, err = mycelium.context.set_secret(key, v)  -- true, or false + reason
mycelium.context.get_profile_id()
mycelium.context.set_plugin_status("syncing", "Indexing 1200/5000")  -- ready|syncing|needs_config|error
```

### `mycelium.storage`

JSON files in the plugin's data area (path-traversal blocked). `write_json`
only writes `*.json` names — never the plugin's own `.lua` code or `manifest.yaml`.

```lua
local t, err = mycelium.storage.read_json("state.json")
local err    = mycelium.storage.write_json("state.json", t)
local exists = mycelium.storage.exists("state.json")
```

### `mycelium.image`

```lua
local info, err = mycelium.image.analyze_logo(url)
-- info = { is_dark = bool, norm_w = int, norm_h = int }
```

### Misc

```lua
mycelium.log("message")                 -- into the plugin's log buffer
mycelium.progress("loading", "text")    -- narrate a long resolve to the client
                                        -- status: "" | success | warning | error
mycelium.sleep(milliseconds)
```

---

## Background tasks

A `tasks` entry runs its function on a schedule. At boot, a task whose last run
is older than its `@every` interval runs once immediately; then it follows the
cron. Tasks of the same plugin never overlap; a heavy task run is capped by
`timeout_seconds`.

Use tasks for catalog sync, domain-health checks, or index rebuilds. Report
progress with `mycelium.context.set_plugin_status` so the dashboard and client
show *why* a plugin isn't ready yet.

---

## Network egress

By default every plugin call (scraping, browser, and the downstream stream
relay) is forced through the configured proxy/VPN and **fails closed** if none
is set — the plugin's real IP can't leak by forgetting a flag. Opt out with
`direct_egress: true` for a source on your own LAN or one that blocks the
proxy's exit IP.

The HLS proxy's own upstream fetches use Go's `net/http` by default. If a CDN
rejects that (its TLS/HTTP2 fingerprint isn't a mainstream browser's), an
operator can switch the whole instance to a browser-compatible profile with
`MYCELIUM_HTTP_PROFILE=browser` or the `http_profile` setting.

---

## Installing

- **Dashboard:** ZIP the plugin directory (the folder itself as the single
  top-level entry) and upload it under **Admin → Plugins → Install**.
- **Filesystem:** drop the directory into `plugins/` and restart (or let
  hot-reload pick it up).

A plugin with unfilled required settings stays *stopped* until you configure and
start it from the dashboard.
