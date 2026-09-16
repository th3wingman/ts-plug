# ts-multinet — design, comparison, and continuation notes

> **Status: MVP.** Linux. Runs directly on the host or in a container netns.
> Verified live across three tailnets (TCP, UDP, ICMP). Host mode: persistent
> browser-login nodes, selected resources, systemd-resolved host DNS with an
> `/etc/hosts` fallback, select/forget CLI + web UI. See
> [`cmd/ts-multinet/README.md`](../cmd/ts-multinet/README.md) for the quickstart.

`ts-multinet` runs **several tailnets transparently on one host at the same
time** — the one thing `tailscaled` can't do. Each tailnet is a stock userspace
**tsnet** node; in front of each we run a small gVisor TCP/IP stack on its own
TUN (tun2socks style) and re-dial every connection out over that tailnet. A
built-in DNS responder hands each MagicDNS name a synthetic IP from a
per-tailnet `198.18.0.0/15` (RFC 2544) range, so the kernel steers to the right
TUN with a plain route — no `ip rule`, no `iptables`, no fwmark.

## Architecture

```
app → DNS query → our responder: host.skynet.ts.net → 198.18.1.5 (synthetic, per-tailnet)
                                   remembers 198.18.1.5 → (skynet, host.skynet.ts.net)

app → connect 198.18.1.5:443
  → kernel route: 198.18.1.0/24 dev tsm0       (plain route, no policy routing)
  → OUR gVisor stack terminates the connection (forwarder.go)
  → reverse-lookup 198.18.1.5 → host, resolve host via the tailnet PEER LIST → real 100.x
  → tsnet.Dial(100.x:443)  → TSNET's gVisor stack re-originates over skynet → wire
```

Two userspace TCP/IP stacks sit back-to-back: **ours** (terminating what the
app sent to the synthetic IP) and **tsnet's** (originating the real tailnet
connection). A proxy copy loop bridges them. This is the source of both the
flexibility (works per-tailnet, no kernel mutation) and the cost (throughput).

### Datapaths — two doors, no shared hallway

- **`tsmN` TUN** carries only **outbound transparent traffic**: what container
  apps send toward synthetic `198.18.x` names.
- **Inbound to the node's own `100.x`** (e.g. someone pings our node) rides
  tsnet's WireGuard+netstack path entirely in userspace and **never touches
  `tsmN`**. `tcpdump -i tsmN` will be empty for that; use `tcpdump -i eth0 udp`
  to see the (encrypted) WireGuard.

The node's assigned `100.x` is placed on each TUN as a `/32` so the kernel
sources synthetic-range traffic from it instead of bouncing off the container's
`eth0`.

### Protocols

| | Behavior |
| --- | --- |
| TCP | terminated on the TUN, re-dialed over the tailnet |
| UDP | per-flow relay with a 60s idle reap (UDP never closes itself) |
| ICMP echo | `ping` proxied: probe the real peer over the tailnet (TSMP — works even if it firewalls ICMP), answer with the real RTT; no reply ⇒ genuinely unreachable |

### Name resolution

Three forms all resolve to the same host:

| Form | Example | How |
| --- | --- | --- |
| Full MagicDNS FQDN | `rpi4-sk-01.tail523555.ts.net` | real suffix match |
| Friendly alias | `rpi4-sk-01.skynet` | the tailnet's `domain` (default: its `name`) accepted as an alias suffix, canonicalized to the real FQDN |
| Bare hostname | `rpi4-sk-01` | resolv.conf `search <names>` expands it; tried against each tailnet (containers) |

The responder is **existence-aware**: it only answers if the host is a real peer
on that tailnet (checked via the peer list), and returns **NXDOMAIN** otherwise.
That's what makes bare-name search work — a libc resolver walking its search
list falls through a tailnet's NXDOMAIN to the next, landing on whichever tailnet
actually has the host. Bare names that collide across tailnets resolve in config
order. `check` does the same expansion itself (it gets the raw arg, no libc
search), via `registry.locate`.

### Host DNS (systemd-resolved)

Synthetic IPs are allocated on first query, keyed on the **real FQDN** — both
alias spellings (`host.<domain>` and `host.<suffix>`) canonicalize to the same
FQDN and therefore the same synthetic IP, so DNS, `/etc/hosts`, and the
forwarder's reverse lookup always agree.

On the host (not in containers), `resolved.go` hands each TUN to
systemd-resolved the way `tailscaled` does: `resolvectl dns <tun> 127.0.0.1` +
routing domains `~<suffix>` and `~<domain>`. Everything else on the host keeps
its normal resolvers; a coexisting `tailscaled` and its `tailscale0` link config
are untouched. Registration is idempotent (change-detected via the tailnet
watcher, so `domain` edits land within one poll tick) and reverted on shutdown.

Degrade paths (both warn, never fatal): no systemd-resolved → registration
skipped, `/etc/hosts` block is the only resolution path; `127.0.0.1:53` already
bound → same, and `dns_listen` can move the responder (resolved accepts
`host:port` for per-link DNS).

### Control plane

The daemon serves a JSON API over a unix socket
(`/run/ts-multinet/control.sock`) and, on the host, the same API + an embedded
web UI on a localhost TCP listener (default `127.0.0.1:8123`, knob
`ui_listen`). `status` / `peers` / `check` / `login` / `reload` /
`select` / `forget` / `lock` / `unlock` / `allow-all` / `domain` / `config` are thin clients that
query the **running daemon** — they never spin up their own tsnet stacks
(which would collide on state locks, `:53`, and authkeys). The mutating verbs
patch the config file in place via the hujson AST (`configpatch.go`), so
comments survive, then swap the in-memory confs and re-apply selections.
Run them with `docker exec <container> ts-multinet <cmd>`, or
`sudo ts-multinet <cmd>` on the host.

## ts-multinet vs a full `tailscaled` client

They are different classes of thing. `tailscaled` is a transparent **L3 VPN
client** for the whole OS on **one** tailnet. `ts-multinet` is an L4 (+ICMP)
**proxy** that fakes transparency for **named services** across **N** tailnets.

| | `tailscaled` (full client) | ts-multinet |
| --- | --- | --- |
| Tailnets at once | 1 (profile switch) | **N simultaneously** ← the whole point |
| Datapath | kernel TUN, true L3 passthrough | userspace tun2socks, double gVisor stack |
| Protocols | everything IP carries | TCP, UDP, ICMP-echo only |
| Addressing | any peer by IP **or** name; subnet routes; exit nodes | **name only** (synthetic ranges); literal `100.x` doesn't steer |
| Connection fidelity | native end-to-end (PMTU, ICMP errors, TCP options pass through) | terminated & re-originated; gVisor re-negotiates; ICMP errors/PMTU don't cross |
| Features | Taildrop, Serve, Funnel, exit node, subnet router, Tailscale SSH | outbound proxying only (the tsnet node *could* Serve/Funnel; not wired) |
| Performance | kernel datapath, GRO/GSO; one userspace hop (wireguard-go) | two userspace TCP stacks + copy — fine for SSH/HTTP/admin, not throughput |
| Privilege/footprint | root daemon; mutates global routing/nftables/resolv.conf; one per host | container + `NET_ADMIN`; sealed netns, zero host pollution; ephemeral; N per host |
| Maturity | production | RnD MVP |

**Framing:** per-tailnet, our tsnet nodes *are* real Tailscale clients (real NAT
traversal, DERP, disco, MagicDNS, WhoIs) — we didn't reimplement Tailscale. What
we added is the transparent-access shim (TUN + synthetic DNS + tun2socks) plus
simultaneous multi-tenancy. The trade is native-L3 completeness for
many-tailnets-at-once. If you need one tailnet, all protocols, max fidelity,
`tailscaled` is strictly better; if you need many at once by name from one host
with no global mutation, `tailscaled` can't do it at all.

## Why not XDP / eBPF

The original idea ("hook tsnet to XDP") is a dead end and shouldn't be
revisited as a steering mechanism:

- XDP is **ingress-only** (NIC RX path). Transparent steering is an **egress**
  problem (local apps originating toward tailnet IPs). Wrong hook.
- XDP/eBPF is **Linux-only** — absent on macOS, RX-only on Windows. No portable
  story. The portable answer is "do steering in userspace + per-OS TUN"
  (utun / Wintun / tun), which is exactly what this design does.
- XDP only ever sees the **encrypted** WireGuard UDP; the decrypted tailnet
  traffic lives in userspace (wireguard-go), so XDP can't accelerate it.

AF_XDP could one day be a Linux-only perf knob for the TUN↔userspace I/O path,
but the gVisor double-stack dominates cost, so it wouldn't move the needle yet.

---

# Continuing this work

Everything a fresh session needs to pick this up.

## Where things are

- **Code:** `cmd/ts-multinet/` (one binary). Files: `ts-multinet.go` (main +
  config + subcommand dispatch), `tailnet.go` (per-tailnet bring-up with its
  own cancelable ctx — teardown-friendly —, status watcher, resolver, pinger,
  assigned-IP-on-TUN), `forwarder.go` (gVisor stack, TCP forwarder, ICMP
  interception, packet pump), `udp.go`, `icmp.go`, `dns.go` (responder +
  synthetic-IP registry + allocator + per-tailnet remove), `resolved.go`
  (per-TUN systemd-resolved registration/revert, applied and reverted live),
  `selection.go` (selection resolution, identity pins, Mullvad filter),
  `hosts.go` (/etc/hosts managed block), `control.go` (daemon registry + unix
  socket server + mutation endpoints + `syncTailnets`/`stopTailnet` — the
  diff-based runtime tailnet lifecycle every change funnels through),
  `configpatch.go` (comment-preserving hujson config edits + atomic write),
  `controlclient.go` (CLI clients + formatting), `web.go` + `web/` (embedded
  vanilla-JS UI on the TCP listener), `tun_linux.go` (raw TUN via ioctl + `ip`
  helpers).
- **Branches:** merged to `main`; host-mode work lives on
  `ts-plug/multinet-host-mode`.
- **Auth:** no authkeys — persistent nodes with one-time browser login
  (`ts-multinet login <tailnet>`); state under `state_dir` (default
  `/var/lib/ts-multinet`). Pinned deps: `gvisor.dev/gvisor@v0.0.0-20250205023644`,
  `tailscale.com@v1.94.2`.

## Build & test (host)

```sh
make ts-multinet                     # builds build/ts-multinet (go + module cache on the host)
go vet ./cmd/ts-multinet
go test ./cmd/ts-multinet -count=1   # unit tests: dns domains, config patching, web mux, ...
./scripts/install-ts-multinet.sh --help
make install-ts-multinet             # same installer via make: builds, installs, restarts
```

The host-mode daemon needs root (TUNs, routes, `/etc/hosts`, `resolvectl`) —
install it as a service (`scripts/install-ts-multinet.sh`) and drive it with
`sudo ts-multinet <cmd>` / the web UI. For container-mode runtimes:

```sh
make docker-ts-multinet          # build the alpine runtime image

docker run -d --name tsm --cap-add NET_ADMIN --device /dev/net/tun \
  -v tsm-state:/var/lib/ts-multinet ts-multinet

docker exec tsm ts-multinet login skynet    # one-time browser login; state persists in the volume
docker exec tsm ts-multinet status
docker exec tsm ts-multinet peers rpi4
docker exec tsm ts-multinet check rpi4-sk-01.tail523555.ts.net:22
docker exec tsm ping -c3 rpi4-sk-01.tail523555.ts.net      # ICMP
# UDP round-trip needs an online responder; e.g. socat UDP echo on a reachable box.
```

The image (alpine) ships a toolbox: `dig`, `curl`, `nc`, `tcpdump`, `jq`, `bash`.

## Hard-won gotchas (don't re-debug these)

- **`tsnet.Dial(name)` resolves via the SYSTEM resolver** — which is our
  hijacked DNS — so dialing by name returns the synthetic IP and loops straight
  back into the TUN (a connection storm; ~15k handler calls/sec). **Always
  resolve via the tailnet peer list and dial by real `100.x` IP.** See
  `newTailnetResolver` (`tailnet.go`) + the synthetic-range guard
  (`registry.isSynthetic`).
- **`header.ICMPv4Checksum(h, payloadCsum)` already sums `h[4:]`**, which
  includes the payload. Pass `payloadCsum = 0` when `h` is the full message, or
  the payload is counted twice (tcpdump: `wrong icmp cksum`). See `icmp.go`.
- **`tcpip.Address.AsSlice()` has a pointer receiver** — copy the address to a
  local var before calling, or it won't compile on a returned value.
- **UDP has no close** — the forwarder fires once per 4-tuple; you must reap
  idle flows yourself (`udpIdle` in `udp.go`) or leak endpoints.
- **Two gVisor stacks in the datapath** (ours + tsnet's) cap throughput. Don't
  expect line rate; this is an admin/SSH/HTTP proxy.
- **Container `resolv.conf` `search` is startup-only** — bare-name expansion
  is written once when the daemon hijacks resolv.conf; later `domain` changes
  don't rewrite it (restart the container to pick them up). Host mode is
  unaffected: resolved routing domains re-register live on domain change.
- Assigned IP goes on the TUN for source addressing; inbound-to-self does NOT
  traverse the TUN (see "two doors" above) — a common tcpdump red herring.

## Roadmap / next steps

1. ~~**Host-wide mode**~~ — **done.** Runs on the host with zero system-DNS
   mutation: persistent browser-login nodes, config-selected resources,
   identity pins, and an `/etc/hosts` managed block. The Mullvad exits that
   flood big tailnets are filtered out of listings and selection (adopted from
   Cauldron's connector implementation).
2. ~~**Proper host DNS**~~ — **done.** Per-TUN registration with
   systemd-resolved (routing domains for suffix + custom `domain`), with
   warn-and-continue fallbacks (no resolved → hosts block; port 53 held →
   move `dns_listen`). See `resolved.go`.
3. **macOS / Windows backends** — single-TUN (utun / Wintun), no eBPF. The
   steering core (DNS + synthetic ranges + tun2socks) is already
   device-count-agnostic; only the TUN plumbing differs per OS.
4. **Literal `100.x` IP access** — needs per-app/cgroup disambiguation because
   CGNAT ranges overlap across tailnets. The synthetic-name trick exists to
   dodge exactly this.
5. **Fidelity/throughput** — collapsing the double stack is hard (tsnet *is*
   gVisor); kernel-WireGuard-per-tailnet would be faster but reintroduces
   routing-table collisions (multi-instance tailscaled pain).
6. **IPv6 synthetic range** (currently A-only; AAAA returns empty NOERROR),
   plus user-defined synthetic ranges.
7. ~~**CLI selection commands (`select`/`forget`) and a local web UI / tray**~~
   — **done** for CLI + web UI: `select`/`forget`/`allow-all`/`domain` verbs
   and an embedded localhost web UI (dashboard, selection, logins, reload) on
   `ui_listen`. Runtime tailnet add/remove extends this item and is done too:
   `add`/`remove` (CLI + UI form) drive `syncTailnets`, so every config change
   applies live — only globals (mtu, dns_listen, state_dir, hosts_file,
   ui_listen) still restart the service. A tray app remains unexplored.
   Bare short names on the host (`ping host` without a suffix) would need
   search-domain decisions per host — still open, arguably part of this item.
