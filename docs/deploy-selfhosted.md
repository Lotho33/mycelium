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

```bash
mkdir -p ~/mycelium && cd ~/mycelium
docker login <your-registry>          # credentials Watchtower reuses

# copy next to the compose file:
#   docker-compose.prod.yml
#   cobweb.toml
mkdir -p data

docker compose -f docker-compose.prod.yml up -d --remove-orphans
```

Bundled plugins ship inside the image; the entrypoint syncs them into the
`mycelium_plugins` volume on every start (`MYCELIUM_PLUGIN_SYNC=1`), so a
Watchtower update also delivers plugin changes — no `git pull` on the server.
The volume keeps dashboard-uploaded plugins and per-plugin caches across
updates. A plugin **dropped** from a release stays in the volume; remove it once
with `docker compose exec mycelium rm -rf plugins/<id>`.

### Networking

`network_mode: host` for mycelium/cobweb/redis: the LAN discovery responder
(UDP `:51900`) needs it, and it lets `ResolveStream` autodetect the server's IP
for the video proxy URLs — no `MYCELIUM_SERVER_HOST` to set by hand. On a
bridged setup you must set `MYCELIUM_SERVER_HOST` (env or dashboard) to a
LAN-reachable address.

### Checks

```bash
docker compose -f docker-compose.prod.yml ps      # all "healthy"
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

`microwarp` (Cloudflare WARP egress) starts with the stack. It is available but
never forced — each plugin picks its exit in the dashboard under **Settings →
VPN → Network exits** (`Direct` is the default). Disable it with
`docker compose -f docker-compose.prod.yml up -d --scale microwarp=0`.

For a generic WireGuard exit, drop a `.conf` onto the "Network exits" page —
mycelium creates a dedicated `wireproxy-<name>` sidecar over the Docker socket
(userspace, no `cap_add`), reusing the mycelium image itself.

## Notes

- The dashboard's "update available" banner queries
  `api.github.com/repos/Lotho33/mycelium/releases/latest`. It's cosmetic,
  not the updater.
- The image is multi-arch (`linux/amd64` + `linux/arm64`) — an ARM server
  pulls the right variant automatically, no compose changes needed.
- Mounting `/var/run/docker.sock` into `mycelium` (for dashboard metrics and
  wireproxy sidecars) is root-equivalent on the host — keep that in mind on an
  exposed machine.
