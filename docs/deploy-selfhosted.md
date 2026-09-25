# Self-hosted deploy

`docker-compose.prod.yml` runs mycelium from a **prebuilt image** (rather than
building from source like `docker-compose.yml`) and adds
[Watchtower](https://containrrr.dev/watchtower/) so a new release tag rolls out
on its own.

```
tag vX.Y.Z ──push──▶ release workflow ──▶ docker build + push
                                          <registry>/mycelium:X.Y.Z + :latest
                                                       │
                                     server  ◀──pull── Watchtower (every ~5 min)
                                                       recreates `mycelium`
```

## Images

`docker-compose.prod.yml` reads the image refs from the environment:

| var             | default                     | meaning |
|-----------------|-----------------------------|---------|
| `MYCELIUM_IMAGE`| `ghcr.io/lotho33/mycelium`  | mycelium image (no tag) |
| `MYCELIUM_TAG`  | `latest`                    | tag to run / pin |
| `COBWEB_IMAGE`  | `ghcr.io/lotho33/cobweb`    | companion browser service image |
| `COBWEB_TAG`    | `latest`                    | tag to run / pin |

Put overrides in a `.env` file next to the compose file to point at your own
registry.

## Releasing

`scripts/release.sh X.Y.Z` bumps `internal/core/version.go`, commits, tags
`vX.Y.Z`, and pushes. The push triggers `.github/workflows/release.yml`,
which builds `ghcr.io/lotho33/mycelium:X.Y.Z` + `:latest` (multi-arch,
`linux/amd64` + `linux/arm64`) and cuts a GitHub Release — GitHub-hosted
runner, no self-hosted runner or registry secret needed (`GITHUB_TOKEN`
covers GHCR push + release creation).

`MYCELIUM_RELEASE_REMOTE` (default `origin`) and `MYCELIUM_RELEASE_BRANCH`
(default `main`) override where the script pushes.

The Go SDK is vendored at `third_party/stipes-sdk/` (a `replace` in `go.mod`),
so the build needs only this one repo — no sibling checkout.

## Server setup

Everything comes from this repo; the images (mycelium, cobweb) are public, so
no registry login is needed. On a fresh Debian host / Proxmox CT with Docker
and the compose plugin installed (for an LXC CT: unprivileged, features
`nesting=1,keyctl=1`):

```bash
apt install -y git
git clone --depth 1 https://github.com/Lotho33/mycelium /opt/mycelium
cd /opt/mycelium
cp deploy/stack.env.example .env       # COMPOSE_FILE = prod + cobweb overlay

# Watchtower mounts ~/.docker/config.json: it must exist as a FILE before the
# first `up`, or Docker creates a directory there and Watchtower breaks.
mkdir -p ~/.docker && [ -f ~/.docker/config.json ] || echo '{}' > ~/.docker/config.json

mkdir -p data/cobweb
docker compose up -d
```

What runs: `mycelium`, `redis`, `cobweb` (`docker-compose.cobweb.yml`, config
in `deploy/cobweb/cobweb.toml`, loopback-only) and `watchtower`, which pulls
new mycelium/cobweb images on its own. To pick up changes to the compose
files themselves: `git pull && docker compose up -d`.

Then open `http://<server-ip>:8000/setup` from the LAN (or your tailnet) to
set the admin password, and install plugins from the dashboard.

### Networking

`network_mode: host` for mycelium/cobweb/redis: the LAN discovery responder
(UDP `:51900`) needs it, and it lets `ResolveStream` autodetect the server's IP
for the video proxy URLs — no `MYCELIUM_SERVER_HOST` to set by hand. On a
bridged setup you must set `MYCELIUM_SERVER_HOST` (env or dashboard) to a
LAN-reachable address.

### Checks

```bash
docker compose ps                                  # all "healthy"
curl -s localhost:8000/health                      # mycelium
curl -s localhost:8191/health                      # cobweb
docker logs -f watchtower
```

## Pin / rollback

Watchtower follows `:latest`. To freeze at a version:

```bash
echo 'MYCELIUM_TAG=1.3.2' >> .env
```

comment out the `com.centurylinklabs.watchtower.enable=true` label on the
`mycelium` service, then `docker compose -f docker-compose.prod.yml up -d
mycelium`. Reverse both to resume auto-update.

## VPN / egress

The public stack has no VPN egress container: every plugin marked
`direct_egress: true` goes out directly, and `VPN_PROXY_URL` is left empty in
`deploy/stack.env.example`. Point it at a SOCKS5 proxy only if you run one.

For a WireGuard exit, drop a `.conf` onto the dashboard's "Network exits"
page — mycelium launches its own bundled `wireproxy` binary as a plain child
process (userspace, no `cap_add`, no `/dev/net/tun`, no Docker socket), then
pick it per plugin.

## Notes

- The dashboard's "update available" banner queries
  `api.github.com/repos/Lotho33/mycelium/releases/latest`. It's cosmetic,
  not the updater.
- The image is multi-arch (`linux/amd64` + `linux/arm64`) — an ARM server
  pulls the right variant automatically, no compose changes needed.
- `mycelium` itself no longer needs `/var/run/docker.sock` at all (dashboard
  metrics and the WireGuard egress sidecars both used to require it; both are
  gone — see the WireGuard note above). Only `watchtower` still mounts the
  socket, for its own unrelated job of recreating containers on a new image
  tag — root-equivalent access, unavoidable for what it does, worth keeping in
  mind on an exposed machine.
