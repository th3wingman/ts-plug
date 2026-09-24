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
      // "auto_select": ["tag:infra", "svc:prod-*"],  // or: select by rule — see below
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

- **Off/on without unprovisioning.** `sudo ts-multinet disable <tailnet>`
  stops the node — state and login kept — and it shows as `disabled` in
  `status` (distinct from `parked`, a failed start); `enable` starts it again
  straight back to Running. Same as the `"enabled": false` config flag and
  the Settings toggle; nothing about resources or node state is touched.

- **Auth keys for tagged devices (optional).** Default is still one-time
  browser login: `ts-multinet login <tailnet>` (prints a URL); state persists
  under `state_dir` and survives restarts. For tagged / automation nodes,
  skip the browser: pass `auth_key` when adding the tailnet (API or the
  Settings field), or `sudo ts-multinet login <tailnet> tskey-auth-…` — the
  node enrolls tagged per the key, no browser. The key is stored in the
  config (0600) and never shown again: `config` output and `GET /config`
  serve it redacted, and since every mutation reloads the real file, updates
  can't wipe it. `TS_AUTHKEY`-style env vars stay refused — an ambient key
  would enroll into the wrong tailnet.
- **Selection** — per tailnet, `resources` lists the short names you care
  about (as `peers` shows them); `"allow_all": true` selects every peer
  instead. Shared Mullvad exit peers are never selectable. Selections are
  **identity-pinned**: the daemon records each peer's stable node key, so a
  renamed or replaced peer under the same name fails loudly instead of
  silently redirecting (delete the pin in `<state_dir>/<tailnet>/selections.json`
  to re-select).
- **Locked essentials** — `locked` pins a subset of `resources` (the
  jumpboxes, logging, metrics you must not lose): a locked selection stays
  selected through `clear`/clear-all, refuses `forget`, and blocks
  `disable`/`remove` of the tailnet carrying it. `sudo ts-multinet
  lock/unlock <tailnet> <peer|svc:label>`, or the lock button on a row in
  the UI. Locking also selects; unlocking keeps the selection — forget is
  the separate step.
- **Advertised services** — Tailscale `svc:<label>` VIP services are
  selectable alongside peers: `resources` accepts `svc:<label>` (or
  `sudo ts-multinet select <tailnet> svc:<label>`, or the UI's **Services**
  tab), and the name resolves under both spellings — `<label>.<domain>` and
  `<label>.<suffix>` — to its own synthetic IP. Discovery is ACL-gated: only
  services this node may use appear in `ts-multinet services <tailnet>`. Unlike
  peers, services are not identity-pinned (the `svc:` name is the identity)
  and are never port-probed — the advertised ports are shown as metadata.
- **Auto-select rules** — `auto_select` selects without listing:
  `tag:<acl-tag>` grabs every peer carrying that Tailscale ACL tag,
  `svc:<glob>` every advertised service whose label matches (Tailscale
  exposes no tags on services, so the label glob *is* the service "tag"),
  and `tcp:<port>` / `udp:<port>` every service advertising that port —
  "all MySQL" is just `tcp:3306`, "all SSH" `tcp:22` (the rule must be
  fully covered by an advertised port range, so a wide rule can't grab a
  service it doesn't really apply to).
  Rules stay rules in the config — matches are recomputed on every apply, and
  a netmap subscription re-applies the moment control pushes a change: tag a
  device in the admin console (or publish a new service) and it lands on this
  host without a reload; untag it and it drops out. Tag-matched peers are
  identity-pinned like explicit selections; `forget` refuses a rule-matched
  name and names the rule (`ts-multinet auto <tailnet> off <rule>` removes
  it); `clear` clears the rules along with resources and allow-all. Manage
  them with `ts-multinet auto <tailnet> [rule...]` / `auto <tailnet> off
  [rule...]` (bare `off` clears all; bare `auto <tailnet>` lists), the
  Settings tab's rule chips, or the config file — the verb's reply names what
  the rules currently match, so a typo'd glob surfaces at write time.
- **`/etc/hosts` managed block** — every selected resource gets a line between
  `# ts-multinet begin` / `# ts-multinet end`, pointing the resource's name at
  its synthetic IP. Names are written bare by default — like any other
  /etc/hosts entry, so completion and lookups work with the plain hostname:

```
198.18.1.2 m4-deb13-hermes  # ts-multinet (skynet)
198.18.2.1 falcon-ui        # ts-multinet (msinfra)
```

  A name two tailnets share is written qualified on both sides — an ambiguous
  bare name would resolve by file order:

```
198.18.1.40 sandbox.skynet  # ts-multinet (skynet)
198.18.2.10 sandbox.msinfra # ts-multinet (msinfra)
```

  Everything outside the markers is preserved. IPs are allocated in sorted
  name order, so they're stable across restarts. The per-tailnet
  `"domain_hosts"` option (Settings-tab checkbox, or the config) forces the
  `.<domain>` suffix on every entry — always off by default.

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
  checkboxes, a **tags** column whose chips toggle `tag:` auto-select rules in
  one click, a **clear all** button (unselects every peer *and* service on the
  tailnet in one write), and a **probe** button that is the *only* path that
  dials ports (the 5s poll never does). **Services**
  (`#/tailnet/<name>/services`): advertised VIP services with their VIP,
  advertised ports — each a one-click `tcp:`/`udp:` auto-select rule toggle
  ("all MySQL" is one click on `tcp:3306`), a name/display filter, selection
  checkboxes, and **clear all** (ACL-gated; never probed). **Settings**
  (`#/tailnet/<name>/settings`): domain, hostname, allow-all, resource chips,
  auto-select rule chips (`tag:x` / `svc:glob` / `tcp:port`, added and removed
  in place, with the add field autocompleting from the tailnet's live tags,
  ports, and service names), plus structural **cidr/tun** — applying those
  restarts just that tailnet (node state and login survive). **restart tailnet**
  stops and starts the node in place — the manual recovery if an outage left
  it dark; no re-login. Danger zone removes the tailnet, keeping node state.
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

That hosts line lists one spelling per host by default — the bare hostname,
qualified with `.<domain>` only when another tailnet claims the same name —
so shell hostname completion offers a single deterministic name. The
per-tailnet `"domain_hosts"` option (Settings-tab checkbox, or the config)
forces the suffix always; `"native_dns"` adds the full MagicDNS name to the
line. DNS resolves every spelling regardless — these options only change
what /etc/hosts lists.

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
sudo ts-multinet restart skynet                 # stop/start one node in place (state kept) — outage recovery
sudo ts-multinet select skynet rpi4-sk-01       # expose a peer (patches the config in place)
sudo ts-multinet select skynet svc:my-db        # expose an advertised service too
sudo ts-multinet lock skynet rpi4-sk-01         # pin an essential: survives clear-all, blocks disable
sudo ts-multinet unlock skynet rpi4-sk-01        # release the pin (still selected until forgotten)
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
sudo ts-multinet auto skynet tag:infra svc:prod-*   # auto-select by rule (tags / service globs)
sudo ts-multinet auto skynet off tag:infra      # remove one rule; bare `off` clears all
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

**Recovery.** tsnet normally reconnects on its own after a network outage, but
a node can get stuck (backend not Running, the control-plane poll never
completing) or its datapath can die. The daemon watches each tailnet's
control-plane connectivity and, after ~90s dark (or immediately if the
datapath died), restarts that node in place — state and login are kept. A
resume from suspend or a network switch is detected directly (an uptime jump
past the wall clock, or the default route changing): once the link settles
(~15s) dark tailnets are restarted right away instead of waiting out the
grace window, with a longer pong timeout for cold paths — recovery after
opening the laptop is ~a minute, not two. The manual equivalent is `sudo
ts-multinet restart <tailnet>` (or the **restart tailnet** button on the
tailnet's Settings tab in the UI).

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
- **Bare names resolve via the hosts block** — `/etc/hosts` lists them bare
  (cross-tailnet collisions get the `.<domain>` suffix); the qualified forms
  (`nucbox.skynet`, `nucbox.tailXXX.ts.net`) resolve system-wide via
  systemd-resolved (or the hosts block without it). In containers, resolv.conf
  `search` expands bare names across the tailnets.
- **IPv4 synthetic only.** AAAA queries return empty so clients fall back to A.
