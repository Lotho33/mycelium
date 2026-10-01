# stipes-sdk

Protocol definitions shared by the mycelium server and its client apps.

- `proto/` — the gRPC API: `auth.proto` (device pairing, profiles, profile
  PIN), `media.proto` (catalog, details, streams, playback, progress,
  downloads), `mycelium.proto` (shared types, browser-service messages).
- `sdk/gen/` — the Go bindings generated from `proto/` (`protoc-gen-go`,
  `protoc-gen-go-grpc`). Regenerate with `scripts/regen-proto.sh` from the
  mycelium repo root.

Client apps copy the `.proto` files from here; this directory is the single
source of truth for the wire contract.
