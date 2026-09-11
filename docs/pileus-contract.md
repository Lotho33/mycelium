# Pileus client contract

Cross-repo contract notes between mycelium (this repo) and the Pileus client.
Covers wire behaviour that isn't captured by the `.proto` files alone. Extend
it as other contracts need pinning down.

---

## ResolveStream — progress contract

`MediaService.ResolveStream` (`proto/media.proto`, served by
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

### Phases the server narrates

1. **Plugin resolve** (`b.ResolveStream(...)`): embed page load, stream
   extraction, upstream URL/token construction. Lua plugins narrate this
   themselves via `mycelium.progress(status, message)` — arbitrary
   plugin-authored text, `status` one of the 4 values above.

2. **Skip-time lookup** (anime only, when not cached): a single
   `{progress, status:"", message:"Ricerca tempi salta intro/outro…"}`, then
   backgrounded — never blocks `{result}`.

3. **HLS head pre-buffer** *(new — 2026-08-30)*: before `{result}`, for HLS
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
   previously-blind buffering wait now has a readable `N/M` readout.

   `status` is always `loading`. No proto change. An older Pileus build that
   only reads `message` shows the text and behaves exactly as before.

### Where the "press play → picture" time goes

- `resolved_url` returned in `{result}` points at **mycelium's own HTTP proxy**
  (`http://<host>/proxy/playlist.m3u8?…` for HLS, `…/proxy/segment.ts?…` for
  progressive) for every plugin **except** those with `direct_stream: true`
  (e.g. jellyfin), which get a raw upstream URL. So for every proxied stream
  (VOD and live alike) mycelium proxies every byte and has full visibility
  into the buffering phase.
- mycelium does **not** transcode/remux — `ProxySegment` is a byte pipe
  (`io.Copy`), with only a cheap MPEG-TS-sync fix for image-wrapped segments.
- Inside `ResolveStream` (before `{result}`): plugin resolve + optional skip
  lookup. This is already narratable.
- After `{result}` (previously invisible): the player opens `resolved_url`,
  mycelium fetches + rewrites the m3u8, then the player pulls the first several
  segments through `/proxy/segment.ts` — one upstream GET each. On weak boxes +
  slow CDN this is the tens-of-seconds wait. The pre-buffer step moves the
  first N of those fetches *before* `{result}` so they can be measured and
  reported, and serves them warm afterwards.

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
