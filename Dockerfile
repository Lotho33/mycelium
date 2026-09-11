FROM golang:1.26 AS builder
WORKDIR /build

# Build tools
RUN go install github.com/swaggo/swag/cmd/swag@latest

# Download Go modules (layer cache). The SDK is vendored in-repo at
# third_party/stipes-sdk (go.mod: replace github.com/Lotho33/stipes-sdk =>
# ./third_party/stipes-sdk), so its go.mod must be present for `go mod
# download` to resolve the replace.
COPY go.mod go.sum ./
COPY third_party/ ./third_party/
RUN go mod download

# Build
COPY . .
RUN swag init -g cmd/server/main.go --output docs

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X mycelium/internal/core.Version=${VERSION}" \
    -o server \
    ./cmd/server

# wireproxy — the userspace-WireGuard → SOCKS5 proxy used by the Fase C egress
# sidecars. Built here (static, CGO off) and shipped inside THIS image so the
# sidecars can run `mycelium:<tag>` itself as their image: no third-party
# registry to pull (ghcr's wireproxy packages 403 on some hosts' Docker auth),
# it's already on the box. Pinned; bump deliberately.
RUN CGO_ENABLED=0 go install github.com/windtf/wireproxy/cmd/wireproxy@v1.1.3

# ── runtime ──────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata wget \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /build/server ./server
COPY --from=builder /go/bin/wireproxy /usr/local/bin/wireproxy
COPY web/ ./web/
COPY entrypoint.sh ./entrypoint.sh
# Bundled plugins ship inside the image (source of truth for what a release
# carries). They land in ./plugins-bundled, NOT ./plugins — the latter is a
# volume in prod; the entrypoint syncs bundled → plugins on start so an image
# update actually delivers plugin changes without shadowing user-uploaded
# plugin dirs or the per-plugin runtime caches. The regenerated caches
# (catalog_cache.json / logo_cache.json / fribb_index.json / avail_cache.json)
# are kept out of the image by .dockerignore.
COPY plugins/ ./plugins-bundled/

RUN mkdir -p plugins data && chmod +x entrypoint.sh

ENV MYCELIUM_DOCKER=1

VOLUME ["/app/plugins", "/app/data"]
EXPOSE 8000 50051

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- http://localhost:8000/health || exit 1

ENTRYPOINT ["./entrypoint.sh"]
