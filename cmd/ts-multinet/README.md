# ts-multinet

Run several tailnets transparently on one host at the same time.

## Why you want this (the short version)

**The problem:** your machine can only be in one tailnet at a time. Need to
reach a host on skynet and then a host on corp? You switch profiles back and
forth, all day. That sucks.

**ts-multinet fixes that.** It connects to *all* your tailnets at once and
writes the hosts you care about into `/etc/hosts`. After that, from any app,
at the same time:

```sh
ssh nucbox.skynet            # tailnet 1
curl http://zombie.msinfra    # tailnet 2
ping rpi4-sk-01.corp         # tailnet 3
```

No profile switching. No auth keys to rotate. Your normal `tailscaled` (if
you run one) keeps working untouched.

**Getting there is three steps, once:**

```sh
sudo scripts/install-ts-multinet.sh   # installs + starts the daemon
sudo ts-multinet login skynet          # prints a link; open it, log in — forever
sudo ts-multinet login corp            # …once per tailnet
```

Then say which hosts you want (in `/etc/ts-multinet/config.json`, one line per
host), run `sudo ts-multinet reload`, and the names work everywhere. See
**[Install](#install)** below for the details, or
`sudo ts-multinet peers` to browse what's out there before deciding.

> **Status: MVP.** Linux. Runs directly on the host (recommended) or inside a
> container network namespace. TCP, UDP, and ICMP-echo work. Nodes are
> persistent with one-time browser login; selected resources land in
> `/etc/hosts`.
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
`/etc/ts-multinet/config.json`, comments included — the loader takes HuJSON):

```jsonc
{
  "state_dir": "/var/lib/ts-multinet",
  "tailnets": [
    {
      "name": "example",              // short id: `login`/`peers`/`reload <name>`
      "cidr": "198.18.1.0/24",        // synthetic range, unique per tailnet
      "tun": "tsm0",                   // <= 15 chars
      // "resources": ["host1", "host2"],  // short names as `peers` shows them
      // "allow_all": true,                // or: every peer (Mullvad exits never)
    },
  ]
}
```

The example documents every optional key (mtu, dns_listen, upstream_dns,
hosts_file, suffix, enabled) inline with its default.

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

## Install

One command from a clone — builds, installs the binary + config + systemd
unit, and starts the daemon:

```sh
sudo scripts/install-ts-multinet.sh
sudo ${EDITOR:-nano} /etc/ts-multinet/config.json   # tailnets, cidrs, resources
sudo systemctl restart ts-multinet
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
sudo ${EDITOR:-nano} /etc/ts-multinet/config.json   # tailnets, cidrs, resources
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

Selection changes: edit the config, then `sudo ts-multinet reload` (or
`sudo systemctl kill -s HUP ts-multinet`). Structural changes (adding a
tailnet, changing `cidr`/`tun`) need a service restart.

## Run (host)

The daemon needs root for the TUNs, routes, and `/etc/hosts` — installed as
above, or for a quick throwaway run:

```sh
sudo ts-multinet -config /etc/ts-multinet/config.json &
sudo ts-multinet login skynet     # prints a URL; open it, authenticate, done — forever
sudo ts-multinet login msinfra
sudo ts-multinet status           # states, assigned IPs, selections per tailnet
```

On the host the daemon **never touches system DNS** — selected resources
resolve via the `/etc/hosts` block, and it coexists with your regular
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
so every peer resolves, selected or not.

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
docker exec tsm -ports 22,5432,3000 ts-multinet peers db
docker exec tsm ts-multinet check rpi4-sk-01.tail523555.ts.net:22
docker exec tsm ts-multinet reload              # after editing selection config
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
- **Host mode resolves selected resources via /etc/hosts.** Bare short names
  (`ping nucbox`) don't expand on the host — use the alias form
  (`nucbox.skynet`). Proper host DNS (responder registered with
  systemd-resolved) is on the roadmap.
- **IPv4 synthetic only.** AAAA queries return empty so clients fall back to A.
