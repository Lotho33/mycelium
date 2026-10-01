#!/bin/sh
# Minimal static ffmpeg for offline downloads (internal/downloads).
#
# It only ever muxes LOCAL files: HLS media playlists written by the
# downloader, their segments (MPEG-TS / fMP4 / AAC / MP3 / WebVTT, possibly
# AES-128 encrypted) → one Matroska file, stream copy, subtitles to SRT. So:
# no network, no TLS, no encoders, no filters: a few MB. Cross-compiles for
# arm64 (no emulation).
#
# --disable-iconv is required: a statically linked glibc's iconv still
# dlopen()s the host's gconv modules at runtime, which crashes with a
# different glibc version on the host.
#
# Usage: build-ffmpeg-min.sh <amd64|arm64|amd64-native> <ffmpeg-src-dir> <out-binary>
#   arm64 = cross-compile from an amd64 builder; amd64-native = native build
#   on whatever the builder is (used when an arm64 machine builds for arm64).
set -eu
arch="$1"; src="$2"; out="$3"

cross=""
case "$arch" in
  amd64|amd64-native) ;; # native build (amd64-native: an arm64 builder building for itself)
  arm64) cross="--enable-cross-compile --arch=aarch64 --target-os=linux --cross-prefix=aarch64-linux-gnu-" ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

cd "$src"
# shellcheck disable=SC2086
./configure $cross \
  --enable-static --disable-shared --extra-ldflags=-static --pkg-config-flags=--static \
  --disable-autodetect --disable-everything --disable-network --disable-asm \
  --disable-iconv \
  --disable-doc --disable-debug --disable-ffplay --disable-ffprobe \
  --disable-avdevice --disable-swscale --disable-postproc \
  --enable-protocol=file,pipe,crypto \
  --enable-demuxer=hls,mpegts,mov,aac,mp3,ac3,eac3,webvtt,matroska \
  --enable-muxer=matroska \
  --enable-parser=h264,hevc,av1,vp9,aac,aac_latm,ac3,mpegaudio,opus \
  --enable-decoder=webvtt,h264,hevc,aac,ac3,eac3,mp3 \
  --enable-encoder=srt,subrip \
  --enable-bsf=aac_adtstoasc,extract_extradata,h264_mp4toannexb,hevc_mp4toannexb,vp9_superframe \
  >/dev/null
make -j"$(nproc)" ffmpeg >/dev/null
if [ -n "$cross" ]; then aarch64-linux-gnu-strip ffmpeg; else strip ffmpeg; fi
cp ffmpeg "$out"
