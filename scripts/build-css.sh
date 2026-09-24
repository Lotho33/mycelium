#!/bin/sh
# Rigenera web/static/tailwind.css (dashboard admin, setup, install) dalle
# classi usate in web/templates e web/static/admin.js. Gira in un container
# Node usa-e-getta: nessuna dipendenza da installare sull'host.
set -eu
cd "$(dirname "$0")/.."
RUNTIME=${CONTAINER_RUNTIME:-$(command -v podman || command -v docker)}
"$RUNTIME" run --rm --security-opt label=disable -v "$PWD":/src -w /src docker.io/library/node:22-alpine \
  npx --yes tailwindcss@3.4.17 -c web/tailwind/tailwind.config.js -i web/tailwind/input.css -o web/static/tailwind.css --minify
