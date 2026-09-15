# ts-multinet

Run several tailnets transparently on one host at the same time.

## Why you want this (the short version)

**The problem:** your machine can only be in one tailnet at a time. Need to
reach a host on skynet and then a host on msinfra? You switch profiles back
and forth, all day. That sucks.

**ts-multinet fixes that.** It connects to *all* your tailnets at once and
makes the hosts you care about reachable by name from anywhere on the
machine — through system DNS (systemd-resolved) with the `/etc/hosts` block
as a fallback. After that, from any app, at the same time:

```sh
ssh nucbox.skynet            # tailnet 1
curl http://zombie.msinfra    # tailnet 2
```

No profile switching. No auth keys to rotate. Your normal `tailscaled` (if
you run one) keeps working untouched.

**Getting there is three steps, once — all live, no config editing, no
restarts:**

```sh
sudo scripts/install-ts-multinet.sh   # installs + starts the daemon (empty)
sudo ts-multinet add skynet           # or the web UI at http://127.0.0.1:8123
sudo ts-multinet login skynet          # prints a link; open it, log in — forever
```

Then say which hosts you want — `sudo ts-multinet select <tailnet> <host>` or
the web UI — and the names work everywhere. See **[Install](#install)** below
for the details, or `sudo ts-multinet peers` to browse what's out there before
deciding.

> **Status: MVP.** Linux. Runs directly on the host (recommended) or inside a
> container network namespace. TCP, UDP, and ICMP-echo work. Nodes are
> persistent with one-time browser login; selected resources resolve via
> system DNS (or `/etc/hosts`) and are managed from the CLI or a local web UI.
>
> For architecture, the full `tailscaled` comparison, and continuation notes
> (code map, gotchas, roadmap), see **[docs/ts-multinet.md](../../docs/ts-multinet.md)**.

## How it works

Each tailnet is a stock userspace **tsnet** node; in front of each we run a
small gVisor TCP/IP stack on its own TUN device (tun2socks style) and re-dial
every connection out through that tailnet.

```
curl https://host.skynet.ts.net
        │  (1) DNS query
        ▼
  ┌─────────────┐   host.skynet.ts.net → 198.18.1.5   (synthetic, per-tailnet range)
  │ DNS responder│──────────────────────────────────────────────┐
  └─────────────┘                                                 │ remembers 198.18.1.5 → (skynet, host.skynet.ts.net)
        │                                                         │
        │  (2) connect 198.18.1.5:443                             │
        ▼                                                         │
   kernel route: 198.18.1.0/24 dev tsm-skynet  ← plain route, no policy routing
        │                                                         │
        ▼                                                         │
  ┌──────────────┐  (3) gVisor terminates the TCP conn           │
  │ tsm-skynet   │      looks up 198.18.1.5 ───────────────────────┘
  │ + gVisor fwd │  (4) tsnet.Dial("host.skynet.ts.net:443")  → out over the skynet tailnet
  └──────────────┘
```

The trick that removes the routing mess: every tailnet gets a **disjoint
synthetic range** out of `198.18.0.0/15` (RFC 2544 benchmark space — never seen
in real traffic). Because the ranges don't overlap, the kernel can steer to the
right TUN with a single plain `ip route`. The synthetic IP also uniquely
identifies the tailnet *and* the original hostname, so the forwarder knows what
to dial. No L3 NAT, no real-tailnet-IP bookkeeping — tsnet resolves the name.

## Config

The config says which tailnets you have and which hosts on them you want
reachable. `resources` lists hosts by their short name — exactly what
`ts-multinet peers` shows. `config.example.jsonc` (installed as
`/etc/ts-multinet/config.json`, comments included — the loader takes HuJSON)
ships **empty**: a fresh install starts with no tailnets and everything is
added live from the web UI or the CLI — the file is the record, not the
interface. A hand-written entry (if you prefer files) looks like:

```jsonc
{
  "state_dir": "/var/lib/ts-multinet",
  "tailnets": [
    {
      "name": "example",              // short id: `login`/`peers`/`select <name> ...`
      "cidr": "198.18.1.0/24",        // synthetic range, unique per tailnet
      "tun": "tsm0",                   // <= 15 chars
      // "domain": "example",               // friendly DNS suffix (default: the name)
      // "resources": ["host1", "svc:db"],  // peer short names, or svc:<label> services
      // "allow_all": true,                // or: every peer (Mullvad exits never)
    },
  ]
}
```

The example documents every optional key (mtu, dns_listen, upstream_dns,
hosts_file, ui_listen, suffix, domain, enabled) inline with its default.

- **The daemon owns the config file.** `add`/`remove`/`select`/`forget`/
  `allow-all`/`domain` (CLI and web UI) patch it in place — comments and
  formatting survive — and apply live. Manual edits still work: edit, then
  `sudo ts-multinet reload`. Only globals (mtu, dns_listen, upstream_dns,
  state_dir, hosts_file, ui_listen) need a service restart. Removing a
  tailnet keeps its node state; re-adding logs back in without a browser.

- **No authkeys.** Each tailnet is a persistent node: log it in once with
  `ts-multinet login <tailnet>` (prints a browser URL); state persists under
  `state_dir` and survives restarts. Set `TS_AUTHKEY`-style env vars and the
  daemon refuses to start that tailnet — an ambient key would enroll into the
  wrong tailnet.
- **Selection** — per tailnet, `resources` lists the short names you care
  about (as `peers` shows them); `"allow_all": true` selects every peer
  instead. Shared Mullvad exit peers are never selectable. Selections are
  **identity-pinned**: the daemon records each peer's stable node key, so a
  renamed or replaced peer under the same name fails loudly instead of
  silently redirecting (delete the pin in `<state_dir>/<tailnet>/selections.json`
  to re-select).
- **Advertised services** — Tailscale `svc:<label>` VIP services are
  selectable alongside peers: `resources` accepts `svc:<label>` (or
  `sudo ts-multinet select <tailnet> svc:<label>`, or the UI's **Services**
  tab), and the name resolves under both spellings — `<label>.<domain>` and
  `<label>.<suffix>` — to its own synthetic IP. Discovery is ACL-gated: only
  services this node may use appear in `ts-multinet services <tailnet>`. Unlike
  peers, services are not identity-pinned (the `svc:` name is the identity)
  and are never port-probed — the advertised ports are shown as metadata.
- **`/etc/hosts` managed block** — every selected resource gets a line between
  `# ts-multinet begin` / `# ts-multinet end`, pointing both the friendly alias
  and the full MagicDNS name at the resource's synthetic IP:

```
198.18.1.5 nucbox.skynet nucbox.tail523555.ts.net  # ts-multinet (skynet)
```

  Everything outside the markers is preserved. IPs are allocated in sorted
  name order, so they're stable across restarts.

- `tun` names must be ≤15 chars (kernel `IFNAMSIZ`).
- Non-tailnet DNS is forwarded to the upstream inherited from the original
  `/etc/resolv.conf` (override with `"upstream_dns"`).
- **Custom domains** — each tailnet's `domain` (default: its `name`) is a
  friendly suffix: both `my-server.skynet` and
  `my-server.tail84a2fd.ts.net` resolve to the same synthetic IP, locally.

## Web UI

The daemon serves a small control panel at **<http://127.0.0.1:8123>** (knob:
`ui_listen`) — same API the CLI talks to, rendered for a browser. Drill-down
layout, hash routes:

- **Overview** (`#/`) — read-only landing: a summary line (tailnets, running,
  peers up/total, advertised services, selected) then one row per tailnet
  (state, suffix, domain, hostname, node IP, peers up/total, services,
  selected, DNS-registered dot). Click a row to drill into that tailnet;
  nothing is editable here.
- **Tailnet detail** (`#/tailnet/<name>`) — a breadcrumb back to the overview,
  then **Peers**: name/FQDN filter, a 200-row render cap with a "show all"
  toggle, a **hide inactive** toggle (hides offline peers), per-row selection
  checkboxes, a **clear all** button (unselects every peer *and* service on the
  tailnet in one write), and a **probe** button that is the *only* path that
  dials ports (the 5s poll never does). **Services**
  (`#/tailnet/<name>/services`): advertised VIP services with their VIP,
  advertised ports, a name/display filter, selection checkboxes, and
  **clear all** (ACL-gated; never probed). **Settings**
  (`#/tailnet/<name>/settings`): domain, hostname, allow-all, resource chips,
  plus structural **cidr/tun** — applying those restarts just that tailnet
  (node state and login survive). Danger zone removes the tailnet, keeping
  node state.
- **Config** (`#/config`) — globals (`mtu`, `dns_listen`, `upstream_dns`,
  `ui_listen`, `hosts_file`) with a needs-restart note on the ones that need
  one, the add-tailnet form (cidr/tun/domain/hostname optional), and a
  collapsible view of the effective config. `state_dir` is deliberately not
  editable here — moving it orphans node state; edit the file if you mean it.

The topbar carries a config indicator: green **config applied** when the
running config matches the file, amber **config changed — Reload** when the
file was edited after the last apply (click Reload). Tailnet-level changes
apply immediately — the per-action note ("config updated; selections
re-applied") is the confirmation; only globals need a service restart.

It binds on localhost only, no auth: same trust model as the unix control
socket (root-owned, local-only). Manage remotely over SSH port-forwarding.
The daemon owns the config file — UI and CLI edits patch it in place through
the HuJSON AST, so comments survive and hand edits still apply after `reload`.

## Host DNS (systemd-resolved)

On hosts running systemd-resolved, the daemon registers its built-in DNS
responder per TUN — `resolvectl dns tsm0 127.0.0.1` + routing domains for
the MagicDNS suffix and the custom domain (the same pattern `tailscaled`
uses) — so tailnet names resolve system-wide while everything else keeps its
normal resolvers. It coexists with a regular `tailscaled` (synthetic ranges
never overlap `100.64.0.0/10`; your `tailscale0` link config is untouched).

Two fallbacks keep it non-fatal:

- **no systemd-resolved** (or not on Linux-with-resolved): registration is
  skipped with a warning; selected names still resolve via the `/etc/hosts`
  block.
- **`127.0.0.1:53` already held** (dnsmasq, NetworkManager): the daemon logs
  a warning and continues hosts-block-only. Move the listener with
  `"dns_listen": "127.0.0.1:5353"` — resolved accepts nonstandard ports for
  per-link DNS, so registration then uses `127.0.0.1:5353`.

Every selected resource still gets its `/etc/hosts` line too — the two paths
always agree, pointing at the same synthetic IP.

## Install

One command from a clone — builds, installs the binary + config + systemd
unit, and starts the daemon:

```sh
sudo scripts/install-ts-multinet.sh   # daemon starts empty
# then, live — no config editing, no restarts:
sudo ts-multinet add skynet           # or: open http://127.0.0.1:8123
sudo ts-multinet login skynet          # browser URL, once, forever
```

Existing config and state are never overwritten; `--uninstall` (optionally
`--purge` to drop node identities) removes everything. See
`scripts/install-ts-multinet.sh --help`.

What the script does, if you'd rather do it by hand (Linux only; the binary is
self-contained):

```sh
make ts-multinet                          # builds build/ts-multinet
sudo install -m755 build/ts-multinet /usr/local/bin/ts-multinet
sudo mkdir -p /etc/ts-multinet
sudo cp cmd/ts-multinet/config.example.jsonc /etc/ts-multinet/config.json
# optional, if you prefer files over the UI/CLI: edit the config, then
#   sudo ts-multinet reload      # applies live — tailnets included
```

Run it as a systemd service — it needs root (TUNs, routes, `/etc/hosts`), and
`StateDirectory` matches the config's default `state_dir`:

```ini
# /etc/systemd/system/ts-multinet.service
[Unit]
Description=ts-multinet — several tailnets on one host
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/ts-multinet -config /etc/ts-multinet/config.json
Restart=on-failure
StateDirectory=ts-multinet

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload && sudo systemctl enable --now ts-multinet
journalctl -u ts-multinet -f           # watch the tailnets come up
```

Selection changes: `sudo ts-multinet select/forget <tailnet> <peer>...`,
`sudo ts-multinet reload`, or the web UI — all patch the config in place and
apply immediately. Tailnet add/remove (`add`/`remove`, or the UI form) is
live too; changing `cidr`/`tun` is a remove + re-add away from live as well.
Only globals (mtu, dns_listen, upstream_dns, state_dir, hosts_file,
ui_listen) need a service restart.

## Run (host)

The daemon needs root for the TUNs, routes, and `/etc/hosts` — installed as
above, or for a quick throwaway run:

```sh
sudo ts-multinet -config /etc/ts-multinet/config.json &
sudo ts-multinet login skynet     # prints a URL; open it, authenticate, done — forever
sudo ts-multinet login msinfra
sudo ts-multinet status           # states, assigned IPs, selections per tailnet
```

On the host the daemon never hijacks system DNS: it registers per-TUN with
systemd-resolved (see **Host DNS** above) and coexists with your regular
`tailscaled` (synthetic ranges never overlap `100.64.0.0/10`).

## Run (container)

```sh
# from the repo root
make docker-ts-multinet

docker run -d --name tsm \
  --cap-add NET_ADMIN --device /dev/net/tun \
  -v tsm-state:/var/lib/ts-multinet \
  ts-multinet

docker exec tsm ts-multinet login skynet   # one-time; state persists in the volume
```

Inside the container resolv.conf points at the built-in responder (as before),
with the per-tailnet custom domains in the `search` list, so every peer
resolves — selected or not — and bare short names expand.

Then, in another shell, exercise both tailnets transparently:

```sh
docker exec -it <container> curl -sk https://<host>.skynet.ts.net/
docker exec -it <container> curl -sk https://<host>.othernet.ts.net/
```

Both resolve and connect at the same time, each over its own tailnet, with an
unmodified `curl`.

## Discovering hosts & diagnosing

The daemon serves a control socket; `status`/`peers`/`check` are thin clients
that query it. Run them **inside the running daemon** — no second tsnet stack,
no state/`:53`/authkey collisions:

```sh
docker exec tsm ts-multinet status              # tailnets, states, assigned IPs, selections
docker exec tsm ts-multinet peers               # all hosts + probed services (Mullvad exits hidden)
docker exec tsm ts-multinet peers rpi4          # name filter
docker exec tsm ts-multinet services            # advertised VIP services per tailnet
docker exec tsm -ports 22,5432,3000 ts-multinet peers db
docker exec tsm ts-multinet check rpi4-sk-01.tail523555.ts.net:22
docker exec tsm ts-multinet reload              # after editing selection config
sudo ts-multinet select skynet rpi4-sk-01       # expose a peer (patches the config in place)
sudo ts-multinet select skynet svc:my-db        # expose an advertised service too
sudo ts-multinet --json status | jq '.[] | select(.state=="Running")'
sudo ts-multinet -details peers dev              # FQDN column + services
sudo ts-multinet --json check nucbox.skynet:22   # raw fields for scripting
```

Every command takes `--json` (machine-readable stdout; errors stay on stderr,
exit codes unchanged) and `--details` (extended human output — a per-tailnet
block for `status` with an advertised-services count, the FQDN column for
`peers`, per-service VIP/port blocks for `services`, raw fields for `check`;
the JSON replies already carry these fields). Multi-peer `select`/`forget`
with `--json` emit one object per line.
sudo ts-multinet forget skynet rpi4-sk-01       # stop exposing it
sudo ts-multinet allow-all msinfra on           # every non-Mullvad peer
sudo ts-multinet domain msinfra                 # print the friendly suffix (set: domain msinfra <name>)
sudo ts-multinet config                         # effective config, as the daemon sees it

```

```

== skynet (tail523555.ts.net) — 2 shown, 2 up ==
  STATE NAME            IP              OS     SERVICES
  UP    rpi4-sk-01      100.82.224.14   linux  :22
  UP    rpi4-st-gw-01   100.72.240.52   linux  :22

host:      rpi4-sk-01.tail523555.ts.net
tailnet:   skynet (tail523555.ts.net)
resolve:   rpi4-sk-01.tail523555.ts.net -> 100.82.224.14
result:    OPEN (60ms) — banner: SSH-2.0-OpenSSH_10.2p1 Ubuntu-2ubuntu3.2

```

`check` tells you which step broke: `resolve FAILED` (not a peer), `UNREACHABLE`
(refused/timeout, with the reason), or `OPEN` with the latency — so a slow path
reads as `OPEN (7.2s)`, not a mystery hang.

## Protocols

- **TCP** — terminated on the TUN, re-dialed over the tailnet.
- **UDP** — per-flow relay with a 60s idle reap (UDP has no close).
- **ICMP echo** — `ping host.tailnet` is proxied: we probe the real peer over
  the tailnet (TSMP, works even if it firewalls ICMP) and answer with the real
  round-trip latency. No reply means the host is genuinely unreachable.

Our node's assigned `100.x` is also placed on each TUN, so the kernel sources
synthetic-range traffic correctly instead of bouncing off the container's eth0.

## Limitations

- **Name-based only.** Connecting to a literal `100.x` tailnet IP isn't steered
  — that's the overlapping-CGNAT case the synthetic ranges exist to avoid.
- **Bare short names on the host** (`ping nucbox`) don't expand outside the
  container — use the alias form (`nucbox.skynet`), which resolves system-wide
  via systemd-resolved (or `/etc/hosts` without it).
- **IPv4 synthetic only.** AAAA queries return empty so clients fall back to A.
