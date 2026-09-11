#!/bin/sh
# Regenerate the stipes-sdk Go bindings (sdk/gen/*.pb.go) from proto/*.proto.
#
#   scripts/regen-proto.sh                 → regenerate third_party/stipes-sdk/
#   scripts/regen-proto.sh ../stipes-sdk   → regenerate the standalone checkout
#
# Needs protoc + protoc-gen-go + protoc-gen-go-grpc on PATH — the mycelium-core
# dev container ships them (see .devcontainer/Dockerfile.dev). After editing a
# .proto: run this, then `go mod tidy` + `go build ./...`.
#
# Standalone → vendor flow: edit ../stipes-sdk/proto, run this against it,
# then scripts/sync-stipes-sdk.sh, then build here.
#
# Note: proto files are passed by bare name with --proto_path=proto, never as
# "proto/foo.proto" — the latter makes protoc emit under sdk/gen/proto/.
set -eu

repo_root=$(cd "$(dirname "$0")/.." && pwd)
target="${1:-$repo_root/third_party/stipes-sdk}"
target=$(cd "$target" && pwd)

[ -d "$target/proto" ] || { echo "error: no proto/ under $target" >&2; exit 1; }
cd "$target"

MODULE=github.com/Lotho33/stipes-sdk

for p in proto/*.proto; do
	name=$(basename "$p")
	protoc --proto_path=proto \
		--go_out=. --go_opt=module="$MODULE" \
		--go-grpc_out=. --go-grpc_opt=module="$MODULE" \
		"$name"
	echo "regenerated $name  ($target)"
done
