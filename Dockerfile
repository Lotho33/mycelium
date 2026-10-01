FROM golang:1.26 AS builder
WORKDIR /build

# Download Go modules (layer cache). The SDK is vendored at
# third_party/stipes-sdk (a replace in go.mod), so it must be present for
# `go mod download`.
COPY go.mod go.sum ./
COPY third_party/ ./third_party/
RUN go mod download

# Build
COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X mycelium/internal/core.Version=${VERSION}" \
    -o server \
    ./cmd/server

# wireproxy — userspace WireGuard → SOCKS5, run by mycelium as a subprocess
# for WireGuard egress profiles (internal/managers/wireproxy.go). Pinned.
RUN CGO_ENABLED=0 go install github.com/windtf/wireproxy/cmd/wireproxy@v1.1.3

# ── ffmpeg for offline downloads (internal/downloads) ────────────────────────
# Minimal static build that only muxes local files
# (scripts/build-ffmpeg-min.sh), cross-compiled on the build machine's
# architecture (no emulation).
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS ffmpeg
ARG TARGETARCH
ARG FFMPEG_VERSION=7.1.1
# sha256 of ffmpeg-7.1.1.tar.xz, whose signature was checked against the
# FFmpeg release signing key (FCF9 86EA 15E6 E293 A564 4F10 B432 2F04 D676
# 58D8) when pinned. Bump version and checksum together.
ARG FFMPEG_SHA256=733984395e0dbbe5c046abda2dc49a5544e7e0e1e2366bba849222ae9e3a03b1
RUN arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
    && apt-get update && apt-get install -y --no-install-recommends \
       build-essential ca-certificates curl xz-utils pkg-config \
    && if [ "$arch" = "arm64" ] && [ "$(dpkg --print-architecture)" != "arm64" ]; then \
         apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu libc6-dev-arm64-cross binutils-aarch64-linux-gnu; \
       fi \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /ffmpeg
RUN curl -fsSL -o ffmpeg.tar.xz "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz" \
    && echo "${FFMPEG_SHA256}  ffmpeg.tar.xz" | sha256sum -c - \
    && tar -xJf ffmpeg.tar.xz
COPY scripts/build-ffmpeg-min.sh ./
RUN arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
    && if [ "$arch" = "arm64" ] && [ "$(dpkg --print-architecture)" = "arm64" ]; then arch=amd64-native; fi \
    && ./build-ffmpeg-min.sh "$arch" "/ffmpeg/ffmpeg-${FFMPEG_VERSION}" /ffmpeg/ffmpeg-bin

# ── runtime ──────────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata wget \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /build/server ./server
COPY --from=builder /go/bin/wireproxy /usr/local/bin/wireproxy
# ffmpeg for offline downloads: muxes the fetched HLS segments into one MKV.
COPY --from=ffmpeg /ffmpeg/ffmpeg-bin /usr/local/bin/ffmpeg
COPY web/ ./web/
COPY entrypoint.sh ./entrypoint.sh
# The image ships no plugins: the operator installs them from the dashboard
# (ZIP upload) into the /app/plugins volume.

# Unprivileged user the entrypoint drops to (see entrypoint.sh). The container
# still starts as root only long enough to fix volume ownership.
RUN groupadd --system --gid 10001 mycelium \
    && useradd --system --uid 10001 --gid 10001 --no-create-home \
       --home-dir /app --shell /usr/sbin/nologin mycelium \
    && mkdir -p plugins data && chmod +x entrypoint.sh \
    && chown mycelium:mycelium plugins data

ENV MYCELIUM_DOCKER=1

VOLUME ["/app/plugins", "/app/data"]
EXPOSE 8000 50051

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- http://localhost:8000/health || exit 1

ENTRYPOINT ["./entrypoint.sh"]
