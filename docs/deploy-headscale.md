# Headscale mesh — deploy walkthrough

Coordination point for the multi-node mycelium mesh (friend-run nodes,
caching/WebP delegation — see project notes). Runs on a small public VPS,
never on the home server itself: mycelium's own host sits behind Starlink
CGNAT and can't accept inbound connections, while a mesh coordinator needs a
stable public endpoint every peer can reach. Once two mycelium nodes have
found each other through it, their actual traffic goes peer-to-peer; the
VPS only relays (via the embedded DERP server) on the rare occasion direct
hole-punching fails.

Headscale is only the control plane — it has no client of its own. Every
node (yours and your friends') runs the official Tailscale client, pointed
at this server instead of Tailscale's SaaS.

Files: `deploy/headscale/` (`docker-compose.yml`, `config.yaml`,
`policy.hujson`, `Caddyfile`, `.env.example`).

## 1. Prerequisites

- A VPS with a public IPv4 (any cheap provider — this only carries
  coordination metadata + occasional DERP-relayed traffic, not the actual
  caching/image workload, so a 1-2€/month box is plenty).
- A domain name (or subdomain) with an A record pointing at the VPS's IP —
  needed both for headscale's `server_url` and for Caddy's automatic TLS
  certificate.
- Firewall open on the VPS: `80/tcp`, `443/tcp`, `3478/udp`.

## 2. First boot

```bash
cd deploy/headscale
cp .env.example .env
# edit .env: HEADSCALE_DOMAIN, ACME_EMAIL, and HEADSCALE_TAG (check
# https://github.com/juanfont/headscale/releases and pin a version)

# edit config.yaml: replace <HEADSCALE_DOMAIN> and <VPS_PUBLIC_IP>
# with the same domain/IP as above

docker compose up -d
docker compose logs -f headscale   # watch for a clean start, no config errors
```

## 3. Create yourself as the admin user, invite nodes

```bash
docker compose exec headscale headscale users create admin

# one reusable, time-boxed key per person you invite — don't reuse a single
# key indefinitely, and don't hand out the raw headscale API forever
docker compose exec headscale headscale preauthkeys create \
  --user admin --reusable --expiration 24h
```

Send the printed key (and your domain) to whoever is joining. Anyone
without a key cannot join the mesh — `policy.hujson` also restricts what a
joined node can reach (see below), so a joined node still isn't handed
blanket trust.

## 4. Join a node with Tailscale

On each machine that should be part of the mesh (your home mycelium host,
a friend's):

```bash
tailscale up --login-server=https://<HEADSCALE_DOMAIN> --authkey=<the key from step 3>
```

New nodes register untagged. Tag them from the headscale side after you've
eyeballed who/what just joined — this is the actual "invite gate", separate
from the pre-auth key:

```bash
docker compose exec headscale headscale nodes list         # find the node id
docker compose exec headscale headscale nodes tag -i <id> -t tag:mycelium
```

Only `tag:mycelium` nodes can reach each other at all (`policy.hujson`);
an untagged node is on the tailnet but isolated.

## 5. Verify connectivity

```bash
tailscale status            # see the other nodes and their 100.x.x.x IPs
tailscale ping <other-node>
curl http://<other-node-tailnet-ip>:8000/health   # once both sides are tag:mycelium
```

If `tailscale ping` shows `via DERP` persistently instead of a direct
path, that pair of NATs isn't hole-punching — traffic still works, just
relayed through the VPS instead of peer-to-peer.

## Rotating / revoking access

```bash
docker compose exec headscale headscale preauthkeys list --user admin
docker compose exec headscale headscale preauthkeys expire --user admin --key <key>
docker compose exec headscale headscale nodes delete -i <id>   # kick a node out entirely
```

## What's next

This covers connectivity only (Fase 1). The sibling-node registry inside
mycelium itself (so a node can discover and delegate image-caching/WebP
work to another `tag:mycelium` peer over its tailnet IP) is a separate,
not-yet-built piece — see project memory for the phased plan.
