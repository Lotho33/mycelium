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

# --- offline downloads (optional, off when absent) ---
download:
  enabled: true                 # offer "download" for this plugin's HLS VOD streams
  preferred_hours: "02:00-07:00" # optional: when the source serves its best quality
                                 # (server local time, may wrap midnight) — offered
                                 # as "download in the recommended window"
                                 # (without it the server learns the window by itself
                                 # from the qualities it sees at each hour)
  note: "La sera la qualità scende spesso a 720p"  # optional fixed hint

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
  subtitles = {                                -- optional, HLS only
    { url = "https://…/it.srt", lang = "it", name = "Italiano", default = true },
  },
}
```

`subtitles` (optional) adds external subtitle tracks the upstream doesn't
carry. The HLS proxy injects each one into the master playlist as an
`EXT-X-MEDIA TYPE=SUBTITLES` rendition (a bare media playlist is wrapped in a
one-variant master), fetches the file server-side and serves it as WebVTT —
SRT/WebVTT, plain, gzip or zip, any encoding (non-UTF-8 is read as
Windows-1252). Players and offline downloads see a normal track. A track
whose language the master already has (non-forced) is skipped; a file that
can't be fetched becomes an empty track, never a playback error. At most 4
tracks; ignored for non-HLS streams and `direct_stream` plugins.

`force_refresh` is set by mycelium when a previously-resolved URL was rejected
downstream — skip any cache and resolve fresh.

`extra` is forwarded to the client verbatim — never put secrets in it. Keys
the Pileus client understands:

| key | value |
|-----|-------|
| `skip_times` | JSON array of `{"skipType":"op"\|"ed"\|"recap","interval":{"startTime":s,"endTime":s}}` — intro/outro/recap markers the player offers to skip |
| `duration_sec` | total duration in seconds (decimal string), also used for the server's progress estimate |
| `download_notice` | see below |

`extra.download_notice` (optional): a hint computed at resolve time, shown
next to the download options — e.g. "ora il massimo è 720p, di notte di solito
1080p". Downloads need nothing else from the plugin: qualities, audio
languages and subtitles are read from the HLS master playlist by the server.

---

## The `mycelium.*` SDK

Available as globals inside every plugin function.

### `mycelium.network`

```lua
local res = mycelium.network.get(url [, headers_table [, timeout_seconds]])
local res = mycelium.network.post(url, body [, headers_table [, timeout_seconds]])
-- res = { status_code = 200, body = "…", headers = { ["Set-Cookie"] = "a\nb" }, error = nil }

local body, err = mycelium.network.fetch(url [, { method=, body=, headers=, timeout_seconds= }])

-- In parallel: one res per request, same order. A request is a URL or
-- { url=, method=, body=, headers=, timeout= }.
local rs = mycelium.network.get_many(requests [, { concurrency = 6, timeout = 30, ratelimit = "name" }])
```

Lua runs one thing at a time, so independent requests in a row add up their
latency; `get_many` runs them in Go (at most 32 per call, `concurrency` at a
time, up to 16; 8 MiB per response) and returns when all are done.
`ratelimit` takes a token from that [`mycelium.ratelimit`](#myceliumratelimit)
bucket before each request.

Transport-level failures are retried a few times. Requests go through the
plugin's configured egress (proxy/VPN) unless the plugin is `direct_egress`. A
`direct_egress` plugin can set the header `_proxy = true` on a single request as
a best-effort opt-in.

### `mycelium.browser`

Reaches a real browser via the optional browser service (`MYCELIUM_BROWSER_URL`, API in the README).
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
local n, err = mycelium.json.each(str, "data.items", function(item, i) ... end)  -- false stops
local node, err = mycelium.xml.parse(str)   -- { _tag, _text, _attrs, [1..n] children }
```

Parsed data lives in Lua tables, which cost far more than the JSON text (a
few hundred bytes per small record), and every state of the plugin's pool
holds its own copy. Prefer `json.get` / `get_array` or `gmatch` over parsing
large lists whole, and `json.each` to walk one: it decodes the array at a
dot-separated path (`nil` = the document) one element at a time. To keep a
large dataset for lookups, use [`mycelium.index`](#myceliumindex).
Dashboard → Plugin → **Memoria dei plugin** estimates what each plugin holds,
per state and per global / module-level `local`, plus its indexes.

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

-- Read, or compute and store. fn returns the value (nil = store nothing) and
-- optionally a ttl for this result. hit = from the cache or another state.
local val, hit = mycelium.cache.remember(key, ttl_seconds, function()
    local v = expensive_lookup()
    return v, (#v == 0) and 3600 or nil
end)
```

`remember` stores the value as JSON (tables come back as tables), so use it
alone on its keys rather than mixing it with `get`/`set`. While one state of
the plugin computes a key, the others asking for it wait for that result
instead of repeating the work; if it fails, each computes on its own.

### `mycelium.ratelimit`

Named token buckets shared by every state of the plugin, to keep an external
service under its limit instead of running into 429s.

```lua
mycelium.ratelimit.set("jikan", 1, 3)          -- 1 request/s, bursts of 3 (top level is fine)
if mycelium.ratelimit.wait("jikan", 2) then    -- false if a token is more than 2 s away (default 30, max 60)
    -- call the service
end
mycelium.ratelimit.pause("jikan", 60)          -- e.g. after a 429: no tokens for 60 s
```

`wait` on a bucket that was never `set` returns at once. In a user-facing call
keep the wait short and skip the call when it returns false.

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
local n, err = mycelium.storage.each_json("big.json", "items", fn)  -- like json.each; missing file = 0, nil
```

### `mycelium.index`

Read-only lookup tables kept in Go and shared by every state of the plugin:
each record is stored once as compact JSON (a few times smaller than Lua
tables, and not copied per state) and decoded only when looked up. Build one
with a builder and publish it under a name; publishing replaces the previous
index at once for every state. Indexes live in memory only and are dropped on
a code reload: rebuild them lazily from your own data.

```lua
if not mycelium.index.has("catalog") then
    local b = mycelium.index.builder()
    mycelium.storage.each_json("catalog.json", nil, function(e)
        b:add(e, "id:" .. e.id, e.mal_id and ("mal:" .. e.mal_id))  -- nil/"" keys are skipped
    end)
    local n, err = b:publish("catalog")
end
local e    = mycelium.index.get("catalog", "mal:123")       -- first value added under the key, or nil
local all  = mycelium.index.get_all("catalog", "mal:123")   -- every value, in insertion order
local keys = mycelium.index.keys("catalog", "id:")          -- sorted; for tasks, not hot paths
local info = mycelium.index.info("catalog")                 -- {records, keys, bytes, built_at} or nil
mycelium.index.drop("catalog")
```

Values are tables, strings, numbers or booleans; identical values are stored
once. A number key and its string form are the same key. Every lookup returns
a fresh copy, so changing it doesn't change the index. Limits: 16 indexes per
plugin, 256 MiB (estimated) and 4M records per index.

### `mycelium.image`

```lua
local info, err = mycelium.image.analyze_logo(url)
-- info = { is_dark = bool, norm_w = int, norm_h = int }
local infos = mycelium.image.analyze_logos(urls)   -- up to 32, 3 at a time; infos[i] = info or { error = "…" }
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

By default every plugin call (HTTP, browser, and the downstream stream
relay) is forced through the configured proxy/VPN and **fails closed** if none
is set — traffic never silently goes out directly because a flag was forgotten. Opt out with
`direct_egress: true` for a source on your own LAN or one that blocks the
proxy's exit IP.

The HLS proxy's own upstream fetches use Go's `net/http`, or the optional
browser service's `/v1/fetch` relay when one is configured
(`MYCELIUM_HTTP_PROFILE=standard` forces the direct path).

---

## Installing

- **Dashboard:** ZIP the plugin directory (the folder itself as the single
  top-level entry) and upload it under **Admin → Plugins → Install**.
- **Filesystem:** drop the directory into `plugins/` and restart (or let
  hot-reload pick it up).

A plugin with unfilled required settings stays *stopped* until you configure and
start it from the dashboard.
