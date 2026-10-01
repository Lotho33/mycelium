# Pileus client contract

Cross-repo contract notes between mycelium (this repo) and the Pileus client.
Covers wire behaviour that isn't captured by the `.proto` files alone. Extend
it as other contracts need pinning down.

---

## ResolveStream — progress contract

`MediaService.ResolveStream` (`third_party/stipes-sdk/proto/media.proto`, served by
`internal/pileus/media_handler.go`) is server-streaming:

```
zero or more  ResolveStreamEvent{ progress: ResolveProgress }
exactly one   ResolveStreamEvent{ result:   ResolveResponse }   ← ends the stream
```

…or the RPC returns a gRPC error instead of a `{result}` on outright failure.

### `ResolveProgress` today (unchanged)

```proto
message ResolveProgress {
  string message = 1;   // free text, shown verbatim under the spinner
  string status  = 2;   // "loading" | "success" | "error" | "warning"
}
```

Client rules the server relies on (do not change without a coordinated bump):

| `status`   | client rendering                    |
|------------|-------------------------------------|
| `loading`  | text only (spinner keeps spinning)  |
| `success`  | ✓ green                             |
| `error`    | ✗ red — **informational, does NOT end the RPC** |
| `warning`  | ! amber                             |
| anything else | treated as `loading`             |

- Only the **latest** progress event is shown.
- 30 s client inactivity timeout, reset on every event. The server must keep
  events flowing at least every few seconds during any long phase.
- **Keep-alive (server):** whenever the stream has been silent for 10 s
  (`resolveKeepAliveEvery`, `internal/pileus/resolve_sender.go`) the server
  re-sends the **last** progress event (same `message`/`status`; "Ricerca
  della sorgente…" if the plugin hasn't said anything yet). Clients must
  treat a repeated message as a no-op, not as a new phase.

### Phases the server narrates

1. **Plugin resolve** (`b.ResolveStream(...)`): embed page load, stream
   extraction, upstream URL/token construction. Lua plugins narrate this
   themselves via `mycelium.progress(status, message)` — arbitrary
   plugin-authored text, `status` one of the 4 values above.

2. **HLS head pre-buffer**: before `{result}`, for HLS
   streams that go through mycelium's proxy (i.e. **not** `direct_stream`
   plugins), the server fetches the media playlist + the first few media
   segments itself, warming its proxy cache, and emits:

   | `message`                              | `status`  | when |
   |----------------------------------------|-----------|------|
   | `Preparazione flusso…`                 | `loading` | fetching the master/variant playlist |
   | `Preparazione flusso… N/M segmenti`    | `loading` | after each pre-buffered segment |
   | `Preparazione flusso… (attesa CDN)`    | `loading` | heartbeat while one segment is slow (count didn't advance) |

   Then `{result}`. Net effect: `{result}` arrives a little later (pre-buffer
   target met, or `prebuffer_max_wait_ms` elapsed), but the segments it points
   at are already warm in RAM, so playback starts almost immediately, and the
   buffering wait has a readable `N/M` readout.

   `status` is always `loading`; a client that only reads `message` just
   shows the text.

### Where the "press play → picture" time goes

- `resolved_url` returned in `{result}` points at **mycelium's own HTTP proxy**
  (`http://<host>/proxy/playlist.m3u8?…` for HLS, `…/proxy/segment.ts?…` for
  progressive) for every plugin **except** those with `direct_stream: true`
  (e.g. a LAN media server), which get a raw upstream URL. So for every proxied stream
  (VOD and live alike) mycelium proxies every byte and has full visibility
  into the buffering phase.
- mycelium does **not** transcode/remux — `ProxySegment` is a byte pipe
  (`io.Copy`), with only a cheap MPEG-TS-sync fix for image-wrapped segments.
- Inside `ResolveStream` (before `{result}`): the plugin resolve, narrated by
  the plugin itself.
- After `{result}`: the player opens `resolved_url`,
  mycelium fetches + rewrites the m3u8, then the player pulls the first several
  segments through `/proxy/segment.ts` — one upstream GET each. On weak boxes +
  slow CDN this is the tens-of-seconds wait. The pre-buffer step moves the
  first N of those fetches *before* `{result}` so they can be measured and
  reported, and serves them warm afterwards.

---

## Errors — gRPC status codes

Backend failures are mapped (`wrapInternal`, `internal/pileus/media_handler.go`)
so the client can pick a message and decide whether to retry:

| code | when | message | client |
|---|---|---|---|
| `FailedPrecondition` | the plugin returned `nil, "msg"` | the plugin's `msg`, verbatim (written for the user) | show it, no retry |
| `DeadlineExceeded` | entrypoint exceeded its time budget | "La sorgente non ha risposto in tempo, riprova" | no automatic retry |
| `Unavailable` | no free Lua state (plugin busy) | "Sorgente occupata, riprova tra poco" | Pileus retries once |
| `NotFound` | plugin not loaded / not running | "Plugin non disponibile" | — |
| `Internal` | anything else | technical text | — |

`UpdateProgress` / `DeleteProgress` return `Internal` on a storage failure.

## One device playing per profile

A profile plays on one device at a time; browsing is never limited.

- **`ResolveStream` → `ABORTED`** (reserved for this): "questo profilo è già
  in riproduzione su «TV salotto»". Offer "Guarda qui" and resolve again with
  `take_over = true`: playback moves to this device immediately.
- **The device that lost it** learns it on its next `/proxy` fetch (HTTP 409,
  body "riproduzione spostata su «Telefono»" — within seconds for live) and on
  its next `UpdateProgress` (`playback_elsewhere = true`, `playing_on` = the
  device name — every 15 s for VOD). Stop the player and say so; don't retry
  or take it back by itself.
- **`ReleasePlayback`** when the player closes: the hold goes at once, so
  moving to another device never waits. Without it, the hold lapses ~90 s
  after the last activity.

## Resume from Continue Watching

`ResolveRequest.start_position_sec` (seconds, 0 = from the start): send the
resume point. The server's pre-buffer then warms the segment containing it
(plus the fMP4 init section) instead of the first segments, so the player's
start from that point is served from cache. The client must still *open* the
player at that position (mpv `start`, ExoPlayer seek before prepare) rather
than start at 0 and seek on the first position tick — that is what shows the
first frame before jumping. Ignored for live streams.

## Offline downloads

Prepared on the server (`internal/downloads`), then fetched by the device.

- **Availability**: a plugin lists the `download` capability in `ListPlugins`
  only if its manifest opts in *and* the server has ffmpeg. Live streams and
  non-HLS sources are never downloadable.
- **`GetDownloadOptions(plugin_id, stream_id)`** → `available` (+
  `unavailable_reason` to show as-is), `duration_sec`, `variants` (best
  first, each with `estimated_bytes`, `label`, `bandwidth_is_peak` = estimate
  is an upper bound), separate `audio` and `subtitles` tracks, plugin hints
  (`plugin_note`, `plugin_notice`, `preferred_hours`, `quality_below_usual` +
  `usual_max_height`), `server_free_bytes`, `bytes_per_sec` (past downloads,
  0 = unknown) and `retention_days`. Total estimate = variant + each chosen
  audio track (subtitles ≈ 0).
- **`CreateDownload`**: ids from the options (they encode criteria, so a
  scheduled job re-matches them on a fresh resolve hours later), optional
  `not_before` / `use_preferred_hours`, plus display metadata (title, series,
  poster, season/episode) so the list works offline. Refused with
  `FailedPrecondition` (message for the user) when it doesn't fit the server
  quota/disk margin or isn't downloadable.
- **`ListDownloads`** (poll every few seconds on the downloads page):
  `status` = `queued | scheduled | running | completed | failed | canceled`,
  `progress` 0..1, sizes, `scheduled_for`, `expires_at`, and for completed
  ones `file_url` — signed, valid ~24 h (list again for a fresh one), HTTP
  `Range` supported (resumable device download; a TV can play it directly).
  Container: Matroska with every chosen audio/subtitle track inside.
- **`CancelDownload` / `DeleteDownload`**: on the caller's own downloads.
  Deleting on the server doesn't touch copies already on devices.
- Downloads belong to the active profile (device without one); the profile
  PIN rules apply as for every other call.
- **Whole season**: `CreateDownloads(plugin_id, items[], variant_id,
  audio_ids, subtitle_ids, …)` — ids from one episode's options apply to all
  (they are criteria); returns every item plus `estimated_total_bytes`. Only
  the first item is probed up front; an episode that turns out not
  downloadable fails on its own when its turn comes. No subscriptions: new
  episodes are never added by themselves.
- **Status `paused`**: the server holds downloads while someone is watching
  (setting, on by default) and resumes by itself; a restart also resumes from
  the segments already fetched.
- **Shared files**: two profiles asking for the same stream with the same
  choices share one server file (`shared = true`); deleting one download
  keeps the file while the other still uses it.
- **`upgrade_if_better`** (CreateDownload/CreateDownloads): if the file comes
  out below the quality this source usually reaches, the server retries in
  the recommended hours (max 3 times) and replaces the file only with a
  better one — `upgrade_pending` while it's trying.
- **`preferred_hours_source`**: `plugin` (manifest) or `learned` (the server
  noticed the source serves its best quality in those hours).
- **`AckDownloadFetched(download_id)`**: call it once the device has the
  whole file. With the server option "delete after the device fetched it"
  the server copy goes (`fetched = true` otherwise).

## Plugin result conventions the server relies on

- `get_streams`: an explicit list — even empty — is returned as-is (empty =
  "nothing to play"); only `nil` means a single implicit source
  `{id = media_id}`.
- `get_catalog` / `browse`: returning `{items = {...}, has_more = bool}`
  makes the server pass the plugin's `has_more` through; a plain array falls
  back to `has_more = #items > 0`.
- On an upstream failure return `nil, "msg"`, not an empty list. Empty
  catalog pages are not cached, and a zero count hides a carousel for at
  most 2 minutes, but an error is still the honest answer.
- `resolve_stream`: `headers` / `extra` may be empty tables.
- Long single steps are fine (keep-alive above), but narrate phases with
  `mycelium.progress` so the user sees what is happening.

---

## Config knobs (admin settings / env)

All optional, all with sane defaults. Settable via the admin settings store
(prefix `prebuffer_` is allow-listed) or `MYCELIUM_<UPPER>` env var.

| key                        | env                              | default    | meaning |
|----------------------------|----------------------------------|------------|---------|
| `prebuffer_enabled`        | `MYCELIUM_PREBUFFER_ENABLED`      | `true`     | master off-switch |
| `prebuffer_segments_vod`   | `MYCELIUM_PREBUFFER_SEGMENTS_VOD` | `3`        | segments to pre-fetch for VOD |
| `prebuffer_segments_live`  | `MYCELIUM_PREBUFFER_SEGMENTS_LIVE`| `1`        | segments to pre-fetch for live (0 = skip for live) |
| `prebuffer_max_wait_ms`    | `MYCELIUM_PREBUFFER_MAX_WAIT_MS`  | `8000`     | hard cap on the whole pre-buffer; `{result}` is sent anyway on hit |
| `prebuffer_max_bytes`      | `MYCELIUM_PREBUFFER_MAX_BYTES`    | `12000000` | stop pre-fetching once this many segment bytes are buffered |
| `prebuffer_cache_mb`       | `MYCELIUM_PREBUFFER_CACHE_MB`     | `96`       | ceiling of the shared warm-segment cache (LRU, ~90 s TTL) |

Cancellation: the pre-buffer inherits the RPC's `context.Context`. If the user
backs out of the player mid-resolve, it stops promptly and the RPC returns the
context error instead of a `{result}`.

Scope: HLS only. Progressive (non-HLS, non-`direct_stream`) resolves keep
today's behaviour (blind spinner for the download phase) — an uncommon path.

---

## Proposed follow-up — structured percentage (Option B, NOT yet applied)

The pre-buffer engine already computes `done / total` segments and bytes.
Surfacing that as a determinate bar in Pileus needs two new **optional** fields
on `ResolveProgress`, applied **atomically with regeneration in both repos**
(`mycelium-core/third_party/stipes-sdk/proto/media.proto` and
`pileus-player/proto/media.proto` — keep them byte-identical):

```proto
message ResolveProgress {
  string message = 1;
  string status  = 2;
  double percent = 3;   // 0..100 for a determinate phase; -1 / unset = indeterminate
  string detail  = 4;   // optional, e.g. "3/8 segmenti" or "1.4 MB/s"
}
```

Regen commands (run in the devcontainer — `protoc v3.21.12`,
`protoc-gen-go v1.36.11`, and for Dart `dart pub global activate protoc_plugin`):

```
# mycelium-core
protoc -I third_party/stipes-sdk/proto \
  --go_out=third_party/stipes-sdk/sdk --go_opt=paths=source_relative \
  --go-grpc_out=third_party/stipes-sdk/sdk --go-grpc_opt=paths=source_relative \
  third_party/stipes-sdk/proto/media.proto

# pileus-player
protoc -I proto --dart_out=grpc:lib/core/grpc/generated proto/media.proto
```

Server side is then a ~5-line change in
`internal/pileus/prebuffer.go`: the `emit` closure already has
`p.Done / p.Total` — set `Percent = 100*done/total` and
`Detail = "<done>/<total> segmenti"` on the `ResolveProgress` it builds.
`message` stays populated, so older clients are unaffected. Kept out of this
change set only because the two proto trees can't be regenerated from the
current host toolchain.

---

## LAN discovery (UDP :51900)

How a Pileus client finds mycelium's current address without any config —
robust to the server's LAN IP changing (dynamic DHCP, container reassignment)
and to mDNS being unavailable.

**Server** (`internal/api/discovery.go`, `StartDiscovery`): listens UDP4 on
`:51900`. A datagram whose entire payload is exactly `MYCELIUM_DISCOVER_V1`
gets a JSON reply sent back to the sender:

```json
{ "name": "Mycelium", "version": "1.0.9", "http_port": 8000,
  "grpc_port": 50051, "setup_done": true,
  "grpc_tls": false, "grpc_tls_fingerprint": "" }
```

The client reads the **source IP of the reply** as mycelium's address (not any
field in the body). Anything other than the exact magic is dropped silently.
Replies are rate-limited to one per source IP per 400 ms.

Same payload shape as `GET /pileus/info` (the HTTP path used once the IP is
already known) — keep the two in sync via `discoveryInfo()`.

**Client** (`pileus-player/lib/core/grpc/host_resolver.dart`,
`_discoverViaBroadcast`): binds an ephemeral UDP port, enables broadcast,
sends the magic to `255.255.255.255:51900` and to each interface's
assumed-/24 broadcast, 3 bursts over ~1 s, and takes the first
`{"name":"Mycelium"}` reply's source IP. It races the existing TCP probe of
saved/`mycelium.local`/env/loopback candidates in `resolveGrpcHost`; the UDP
answer wins if it is also TCP-reachable on `grpc_port`.

Deployment: `network_mode: host` (the prod compose) is required for broadcast
to reach the container; bridge mode must publish `51900/udp` (dev compose
does). Known gaps: Android may drop inbound broadcast without a
`WifiManager.MulticastLock` on some OEMs (TCP fallback still works); iOS shows
the local-network permission prompt (already triggered by the TCP LAN probe).
