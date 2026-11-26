# Writing a mycelium plugin

A plugin teaches mycelium about one content source. It is a directory of Lua
files plus a manifest; mycelium loads it into a sandboxed VM pool, schedules its
background jobs, and routes client requests to it through one unified pipeline.

There is no build step and no toolchain: drop the directory in `plugins/`, or
upload a ZIP from the admin dashboard. Editing its files hot-reloads the plugin.

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
id: my_plugin                 # unique; also the directory name of a dashboard install
name: My Plugin               # shown to users
version: 1.0.0
author: you
description: One line about what this plugin provides
icon: icon.svg                # path relative to the plugin dir
min_core: 1.9.0               # optional: oldest mycelium this plugin runs on;
                              # an older server refuses it with a clear error

pool_size: 2                  # concurrent Lua VMs for this plugin (default 2)

# --- network routing (all optional) ---
direct_egress: true           # opt OUT of the default proxy/VPN routing
vpn_optional: true            # (with direct_egress) add an admin toggle that
                              # routes only the video flow through the proxy
direct_stream: true           # hand the resolved URL to the player as-is,
                              # skipping mycelium's HLS proxy (LAN sources only;
                              # one resolve can opt back in with proxy = true)
warm_resolve: true            # resolve_stream caches its result: mycelium may
                              # run it ahead of Play (args.warm = true) when a
                              # details page opens; adds an admin toggle, on
share_live: false             # default true: Plays of a live stream already
                              # being watched reuse its link and the proxy fetches
                              # it once for all; false when the site ties a link
                              # to one viewer (a per-session token)

# --- segments packed in another container (optional, see "Packed segments") ---
segments:
  unwrap:
    - carrier: png_pixels       # body | png_pixels | webp_chunk | png_text
      magic: "MAGIC"            # bytes right before the payload
      length: u32be             # "" | u32be | u32le: length field after magic
      codec: gzip               # "" | gzip | zlib | base64

# --- encrypted segment URIs in media playlists (optional, see "Sealed playlists") ---
playlist:
  sealed_uris:
    cipher: aes-128-ctr         # aes-128-ctr | aes-256-ctr
    counter: media_sequence     # IV = the segment's media sequence number

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
      style_hint: featured             # featured | ranked (a Top 10 row: big
                                       # numbers, at most 10 cards); free-form
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
  calendar:       calendar        # optional: release calendar

tasks:
  - function: sync_catalog
    cron: "@every 12h"          # "@every <dur>" | cron expression | "" (manual)
  - function: refresh_now
    cron: "@every 2m"
    live_refresh: true          # client can also trigger it on demand
  - function: rebuild_index
    cron: ""                    # manual-only: a button in the dashboard
    timeout_seconds: 600        # overrides the 90 s default for this task
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
| `get_streams`    | `media_id`                          | array of stream sources: `{ id, label, quality, is_live, path }` (see below) |
| `resolve_stream` | `stream_id`, `force_refresh` (bool), `warm` (bool) | `{ url, headers, is_live, extra }` |
| `calendar`       | `from`, `to` (`"YYYY-MM-DD"`), `follows` | array of releases (see "Release calendar") |

### Stream sources

```lua
{ id = "a:p2", label = "Channel A · Server 2", is_live = true,
  path = { "Channel A", "Server 2" } }   -- optional
```

`label` is the full name, shown by clients that list sources flat. `path`
(optional) nests them: outermost group first, ending with the source's own
name; any depth, and sources may use different depths (one level for a
channel page, two for an event with several channels). Return the sources
in order of preference, each group's sources together: a client that falls
back after a failure tries the next source in the list, so the rest of the
same group comes first. Choosing a group plays its first source, so put the
one that should start by default (e.g. an automatic choice) first.

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
  rank        = 1,               -- optional: place in a "ranked" row
  badge       = "new",           -- optional: "new" | "new_episode"
  extra       = { fanart_url = "https://…" },
}
```

`rank` (optional) is the number a `style_hint: ranked` row shows; without it
the client numbers the cards by their place. Set it when the plugin drops
items from a row (e.g. those already shown higher up), which shifts places.
`badge` (optional) labels the card: `"new"` (added recently), `"new_episode"`
(a new episode came out recently). Clients ignore values they don't know.

On a series, `get_details` may add the latest episode out:

```lua
latest_episode = { season_number = 2, episode_number = 8, air_date = "2026-11-20" }
```

The server compares it with where each profile following the series got to,
to flag a new episode on their lists. Leave it out when unknown; never fetch
anything extra just for it.

`extra` is opaque to mycelium and reaches the client as is, on catalog items
and on episode rows (`browse` results with `media_type = "episode"`). On an
episode, `extra.lang` names its language versions: `""` / `"both"` (nothing
to flag), `"ita"`, `"jap"`, `"eng"`; `extra.filler` marks it `"filler"` or
`"recap"` (a summary episode) — `""` or absent when canon or not known, and
clients show nothing then. Compute them only from data already at hand — no
extra request per episode.

### Release calendar (`calendar`)

Optional. What comes out between `from` and `to` (dates, inclusive; at most
31 days). Each release is an item (same shape as a catalog item) plus:

```lua
{
  id = "1396_2_8", title = "Episodio 8", media_type = "episode",
  poster_url = "https://…", season_number = 2, episode_number = 8,
  series_id  = "1396",                  -- the series' media id (or show_id)
  release_at = "2026-11-26T15:30",      -- or "2026-11-26" when only the day is known
  available  = false,                   -- already in the catalog (playable)?
}
```

`release_at`: a date, or a date and time with or without seconds and offset
(`"2026-11-26T15:30+09:00"`); without an offset it is the server's time zone.
Releases without a readable date, or outside the range, are dropped.

`available` is the honest part: a date is a plan, and a title announced for
a day may reach the source days later. Say `true` only when the plugin
already has it (e.g. its episode index lists it).

`args.follows` holds the ids of the series the calling profile follows in
this plugin (La mia lista or watched lately): use it to keep the work small
— look up only those, for instance — but the server still marks followed
releases itself. The server keeps each answer for 30 minutes — 2 minutes
when it has `media_type: "live"` items (their `available` means "on air
now"), 5 after a failure — and asks every plugin in parallel; a new plugin
version is asked again at once.

### `resolve_stream` result

```lua
{
  url     = "https://cdn.example/index.m3u8",  -- final playable URL
  headers = { Referer = "…", Cookie = "…" },   -- replayed by the HLS proxy
  is_live = false,
  proxy   = false,                             -- optional: true = through the proxy even if direct_stream
  extra   = { … },                             -- plugin-specific passthrough
  subtitles = {                                -- optional, HLS only
    { url = "https://…/it.srt", lang = "it", name = "Italiano", default = true },
    { data_base64 = "…", lang = "it", name = "Italiano 2" },  -- the file itself
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
tracks; ignored for non-HLS streams and for `direct_stream` plugins unless
the result also sets `proxy = true`.

`data_base64` hands the file itself instead of a `url` (same formats, up to
4 MiB): for a subtitle the plugin keeps on disk, so a later Play needs no new
download. mycelium holds it in memory only; after a restart the track is
empty until the next resolve hands it again.

`force_refresh` is set by mycelium when a previously-resolved URL was rejected
downstream — skip any cache and resolve fresh.

`warm` is set when the resolve runs ahead of Play (`warm_resolve: true` in the
manifest, see `WarmStream` in [pileus-contract.md](pileus-contract.md)):
nobody is watching yet, so fill the cache but skip anything only a real Play
should do, such as remembering the profile's chosen language. The Play that
follows calls `resolve_stream` again and finds the cache warm — do those
per-profile writes on the cache-hit path too. Only for plugins whose
`resolve_stream` caches its result: without a cache a warm doubles the
requests to the site.

`proxy = true` (optional) sends this one stream through mycelium's proxy
even though the plugin is `direct_stream`: e.g. a server-side transcode that
needs `subtitles` injected, next to an original file that stays direct.
Without it a `direct_stream` plugin's URL goes to the player as-is.

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

For completion and type checks in an editor, point the Lua language server
(LuaLS, e.g. the VS Code "Lua" extension) at
[`mycelium.meta.lua`](mycelium.meta.lua) — e.g. a `.luarc.json` in the
plugin folder with `{ "workspace.library": ["<mycelium-core>/docs"] }`. It
covers this whole SDK plus the `plugintest` helpers; a test in the core keeps
it in step with what the server actually registers.

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
local hit_url, headers, err = mycelium.browser.sniff(trigger_url, url_pattern [, timeout_seconds [, { referer = "https://…" }]])
```

`sniff` opens `trigger_url` and returns the first network request whose URL
matches `url_pattern` (glob), plus that request's headers — the usual way to
grab a stream URL a page builds in JavaScript. The optional `referer` is sent
as the Referer of that navigation: some embed players answer a plain 403
when opened without one. Prefer plain HTTP when the page already carries
what you need (e.g. a player config in the HTML): the browser is the slow,
fragile path, so keep `sniff` as the fallback.

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
local b64    = mycelium.crypto.base64_encode(s)                  -- std, binary-safe
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

`cache.get` tells a stored empty string (`"", true`: e.g. a cached "nothing
found") from a missing key (`"", false`). `remember` results are shared by
every profile (the key is per plugin, not per profile).

`remember` stores the value as JSON (tables come back as tables), so use it
alone on its keys rather than mixing it with `get`/`set`. While one state of
the plugin computes a key, the others asking for it wait for that result
instead of repeating the work; if it fails, each computes on its own.

### `mycelium.ratelimit`

Named token buckets shared by every state of the plugin, to keep an external
service under its limit instead of running into 429s.

```lua
mycelium.ratelimit.set("api", 1, 3)            -- 1 request/s, bursts of 3 (top level is fine)
if mycelium.ratelimit.wait("api", 2)   then    -- false if a token is more than 2 s away (default 30, max 60)
    -- call the service
end
mycelium.ratelimit.pause("api", 60)            -- e.g. after a 429: no tokens for 60 s
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
Sub-directories (`"subs/x.json"`) are created on write. Data files survive
plugin updates.

```lua
local t, err = mycelium.storage.read_json("state.json")
local err    = mycelium.storage.write_json("state.json", t)
local exists = mycelium.storage.exists("state.json")
local err    = mycelium.storage.remove("subs/old.json")      -- *.json only; missing = nil
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
local cover = mycelium.image.versus_url(home_badge_url, away_badge_url)  -- or nil
```

Logos need no analysis in the plugin: return the plain image URL as
`logo_url` and the core measures it (once, in the background) and sends
clients `logo_meta` with what they need to keep it readable (halo, aspect
ratio). `analyze_logo(s)` stays for existing plugins; the old
`url|w|h|dark` encoding of `logo_url` still works but is no longer needed.

`versus_url` returns a 16:9 cover the server draws from two badges (home on
the left, each half tinted with its badge's colour), for events without
artwork of their own: use it as `poster_url`. The URL is relative to the
server (`mycelium:/img/versus?…`); every client receives it absolute, with
the address it reaches the server at. Any string a plugin returns that
starts with `mycelium:/` gets the same treatment.

### Misc

```lua
mycelium.log("message")                 -- into the plugin's log buffer
mycelium.progress("loading", "text")    -- narrate a long resolve to the client
                                        -- status: "loading" | "" | success | warning | error
mycelium.sleep(milliseconds)
```

### Limits and timeouts

- Entrypoint calls: 90 s (read entrypoints — search, filters, details,
  browse, streams — 45 s); a task's own `timeout_seconds` overrides it.
- `init.lua`'s top level (and the plugin's modules, loaded at start): 30 s,
  and no network — it should only define functions and tables. The plugin's
  modules are loaded before the first call: define per-call state in locals,
  not in globals (globals a call adds are wiped after it).
- `network.get/post/fetch`: `timeout_seconds` up to 90 s; a response over
  32 MiB (8 MiB per request in `get_many`) is an error, not a cut body. Only
  GET/HEAD are retried on a transport error.
- `string.rep` results are capped at 32 MiB; `coroutine` is not available.
- `storage.*` paths can't leave the plugin directory, symlinks included.

---

## Background tasks

A `tasks` entry runs its function on a schedule. At boot, a task that never
ran, or whose last run is older than its `@every` interval, runs once
immediately; then it follows the cron (a calendar expression runs only at its
times). Tasks of the same plugin never overlap, and a tick that finds the same
task still running is skipped; a heavy task run is capped by
`timeout_seconds`.

Use tasks for catalog sync, domain-health checks, or index rebuilds. Report
progress with `mycelium.context.set_plugin_status` so the dashboard and client
show *why* a plugin isn't ready yet.

---

## Packed segments

Some sources serve HLS segments packed inside another container (often an
image, because their CDN only accepts images) and unpack them in the page's
JavaScript. `segments.unwrap` tells the proxy how to do the same: a list of
rules, tried in order on every segment of the plugin's streams that is not
already MPEG-TS or fMP4.

| field | meaning |
|-------|---------|
| `carrier` | `body`: anywhere in the response, right after `magic` (searched). `png_pixels`: the samples of an 8-bit RGB/RGBA PNG, alpha dropped, row by row from the first pixel. `webp_chunk`: the data of the WebP chunk named `chunk`. `png_text`: the text of the PNG's `tEXt`/`zTXt`/`iTXt` chunks whose keyword starts with `chunk`, joined in file order (compressed ones inflated first) |
| `chunk` | `webp_chunk`: the FourCC, e.g. `EXIF`. `png_text`: the keyword prefix, e.g. `payload-` (empty = every text chunk) |
| `magic` | bytes before the payload; required for `body`, must start the carrier otherwise |
| `length` | `u32be` / `u32le`: a 4-byte length right after `magic`; empty = to the end |
| `codec` | `gzip` / `zlib` / `base64` (standard alphabet, padding and line breaks optional), empty = raw |

What a rule extracts is used only if it is MPEG-TS or fMP4; at most 8 rules,
invalid ones are dropped at load (logged). Sizes declared by the source are
bounded before decoding (canvas, row width, payload <= 32 MiB). The rules
reach the proxy through its own signed URLs, so they apply to playback,
pre-buffer, read-ahead and downloads alike, and only to this plugin.

## Sealed playlists

Some sources encrypt the segment URIs of their HLS media playlists and
decrypt them in the page's JavaScript. `playlist.sealed_uris` tells the proxy
how to do the same; the key changes per stream, so `resolve_stream` appends it
to the playlist URL as a fragment:

```lua
return { url = playlist_url .. "#mpk=" .. key_base64url, headers = {...} }
```

A fragment never goes upstream, and it follows the URL through re-resolves,
variant playlists and the pre-buffer, so a new key comes with every new
token. Each segment line is base64url ciphertext; with `media_sequence` the
16-byte initial counter is zero except for the segment's sequence number
(big-endian, last 8 bytes): `#EXT-X-MEDIA-SEQUENCE` for the first segment
line, +1 for each following one. A line that doesn't decrypt to an http(s)
URL or a path is left as it is, so a playlist served in clear still works.

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

## Testing a plugin

`cmd/plugintest` runs a plugin's own Lua test files against the real
`mycelium.*` SDK, the plugin loaded exactly as on a server, with every
network call answered from routes the test declares — no server, no source
reachable, no installation.

```sh
go run ./cmd/plugintest -plugin ./myplugin -set api_token=x ./myplugin/tests/*.lua
```

`-set key=value` sets one of the plugin's global settings
(`mycelium.context.get_global_setting`); `-v` shows the plugin's log lines.
Each file runs in a fresh state and may call the plugin's entrypoints and
globals directly. Besides those, a test file has:

| | |
|---|---|
| `plugintest.route(pattern, response)` | `pattern` is a Go regexp on the URL; `response` a body string or `{status=, body=, headers={...}}`. The most recently added matching route answers. |
| `plugintest.requests()` | the requests made so far: `{method=, url=, headers=}` |
| `plugintest.clear()` | forget the recorded requests |
| `check(cond, message)` | fails the file when `cond` is false |

A request no route answers gets status 599 and fails the file, listed even
when the test failed for another reason (it is often the cause). There is no
Redis: `mycelium.cache` stays empty, so code behind a cache runs every time.
A test may replace a plugin global (e.g. an availability check) to keep the
test on what it is about.

```lua
-- tests/search_test.lua
plugintest.route("/search", '{"results":[{"id":1,"title":"A"}]}')
local res = search_items({ query = "a", filters = {} })
check(#res.items == 1, "one result")
check(plugintest.requests()[1].url:find("query=a", 1, true), "query sent")
```

Keep tests in a subdirectory (`tests/`): only the `.lua` files at the
plugin's top level are loaded as its modules.

---

## Installing

- **Dashboard:** ZIP the plugin directory (the folder itself as the single
  top-level entry) and upload it under **Admin → Plugins → Install**.
- **Filesystem:** drop the directory into `plugins/` and restart. Edits to a
  loaded plugin are hot-reloaded: its `.lua` files reload its Lua states, a
  changed `manifest.yaml` reloads the whole plugin. A new plugin directory
  needs a restart (or a dashboard install).

A plugin with unfilled required settings stays *waiting* until you configure and
start it from the dashboard.
