#!/bin/sh
set -e

# ─── Privilege drop ──────────────────────────────────────────────────────────
# The server never needs root (ports >1024, writes only under ./data and
# ./plugins). The entrypoint starts as root only to fix the ownership of
# mycelium's own files in the volumes, then drops to
# MYCELIUM_PUID:MYCELIUM_PGID (default 10001, the image's "mycelium" user).
# Started with `user:` (not root): nothing to do. MYCELIUM_RUN_AS_ROOT=1
# skips the drop.
if [ "$(id -u)" = "0" ] && [ "${MYCELIUM_RUN_AS_ROOT:-0}" != "1" ]; then
    PUID=${MYCELIUM_PUID:-10001}
    PGID=${MYCELIUM_PGID:-10001}
    mkdir -p ./data ./plugins

    # ./data may be shared with other containers: take over only mycelium's
    # own files. The directory keeps its owner and becomes group-writable
    # (SQLite -wal/-shm, config.json.tmp and new subdirectories go in it).
    chgrp "$PGID" ./data && chmod g+rwx ./data || \
        echo "[entrypoint] warning: cannot make ./data writable for gid $PGID"
    own() {
        [ -e "$1" ] || return 0
        find "$1" \( ! -user "$PUID" -o ! -group "$PGID" \) -exec chown -h "$PUID:$PGID" {} + || \
            echo "[entrypoint] warning: chown $1 failed"
    }
    for p in ./data/config.json ./data/config.json.tmp \
             ./data/mycelium.db ./data/mycelium.db-wal ./data/mycelium.db-shm \
             ./data/logs ./data/imgcache ./data/pileus-web ./data/pileus-web-tv \
             ./data/wireproxy ./data/downloads ./plugins; do
        own "$p"
    done

    # Keep CAP_NET_BIND_SERVICE (for a server_port <1024) when the container
    # has it. Probed first: with cap_drop: ALL setpriv would fail.
    CAPS=""
    if setpriv --reuid="$PUID" --regid="$PGID" --clear-groups \
        --inh-caps=+net_bind_service --ambient-caps=+net_bind_service true 2>/dev/null; then
        CAPS="--inh-caps=+net_bind_service --ambient-caps=+net_bind_service"
    fi
    echo "[entrypoint] dropping privileges to ${PUID}:${PGID}"
    # shellcheck disable=SC2086
    exec setpriv --reuid="$PUID" --regid="$PGID" --clear-groups $CAPS ./server
fi

exec ./server
