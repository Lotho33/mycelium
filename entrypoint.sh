#!/bin/sh
set -e

# Sync the image's bundled plugins into /app/plugins on start. Enabled only
# where /app/plugins is a volume (prod: MYCELIUM_PLUGIN_SYNC=1); the dev
# compose bind-mounts the repo's ./plugins for live editing and leaves this
# off so a rebuilt image never clobbers uncommitted local plugin edits.
#
# `cp` copies bundled plugin dirs on top of whatever is in the volume
# (bundled plugins = image is source of truth), while leaving untouched:
#   - user-uploaded plugin dirs (not present in plugins-bundled)
#   - per-plugin runtime caches (kept out of the image by .dockerignore, so
#     they simply aren't in plugins-bundled to overwrite)
# A plugin removed from a release still lingers in the volume — delete it
# once by hand: `docker compose exec mycelium rm -rf plugins/<id>`.
if [ "${MYCELIUM_PLUGIN_SYNC:-0}" = "1" ] && [ -d ./plugins-bundled ]; then
    echo "[entrypoint] syncing bundled plugins → ./plugins"
    cp -a ./plugins-bundled/. ./plugins/ 2>/dev/null || \
        echo "[entrypoint] warning: bundled-plugin sync failed, continuing"
fi

exec ./server
