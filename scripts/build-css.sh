#!/bin/sh
# Regenerates web/static/tailwind.css (dashboard, setup, install pages) from
# the classes used in web/templates and web/static/admin.js, in a throwaway
# Node container (nothing to install on the host).
set -eu
cd "$(dirname "$0")/.."
RUNTIME=${CONTAINER_RUNTIME:-$(command -v podman || command -v docker)}
"$RUNTIME" run --rm --security-opt label=disable -v "$PWD":/src -w /src docker.io/library/node:22-alpine \
  npx --yes tailwindcss@3.4.17 -c web/tailwind/tailwind.config.js -i web/tailwind/input.css -o web/static/tailwind.css --minify
