#!/usr/bin/env bash
# Re-syncs third_party/stipes-sdk/ from a stipes-sdk checkout.
#
# mycelium builds against this in-repo copy, not the separate repo:
#   go.mod → replace github.com/Lotho33/stipes-sdk => ./third_party/stipes-sdk
#
# Run this after changing the SDK's Go API (new proto types, etc.), then
# commit third_party/stipes-sdk/ together with the mycelium change that
# needs it. The standalone Lotho33/stipes-sdk repo stays the source of
# truth (Pileus' Dart side consumes it); this is just a vendored snapshot
# of its Go packages.
#
# Usage:  scripts/sync-stipes-sdk.sh [path-to-stipes-sdk]   (default: ../stipes-sdk)

set -euo pipefail

SRC="${1:-$(cd "$(dirname "$0")/.." && pwd)/../stipes-sdk}"
DST="$(cd "$(dirname "$0")/.." && pwd)/third_party/stipes-sdk"

[ -f "$SRC/go.mod" ] || { echo "error: no go.mod under $SRC" >&2; exit 1; }

rm -rf "$DST"
mkdir -p "$DST"
for p in go.mod go.sum LICENSE README.md sdk proto; do
  [ -e "$SRC/$p" ] && cp -R "$SRC/$p" "$DST/"
done
# mycelium consumes only the Go side; drop the Dart protobufs (Pileus' side)
find "$DST" -name '*.dart' -delete

rev="$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null || echo unknown)"
echo "synced third_party/stipes-sdk from $SRC @ $rev"
