#!/bin/sh
# Regenerates web/static/tailwind.css (setup and install pages) from the
# classes used in those templates, in a throwaway Node container (nothing to
# install on the host). The dashboard has its own hand-written
# web/static/admin.css and does not use Tailwind.
set -eu
cd "$(dirname "$0")/.."
RUNTIME=${CONTAINER_RUNTIME:-$(command -v docker || command -v podman)}
"$RUNTIME" run --rm --security-opt label=disable -v "$PWD":/src -w /src docker.io/library/node:22-alpine \
  npx --yes tailwindcss@3.4.17 -c web/tailwind/tailwind.config.js -i web/tailwind/input.css -o web/static/tailwind.css --minify
