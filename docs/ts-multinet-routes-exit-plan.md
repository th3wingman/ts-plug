# ts-multinet: subnet routes and exit nodes — plan

Status: **proposed, not started.** Companion to [ts-multinet.md](ts-multinet.md)
(architecture) and [ts-multinet-plan.md](ts-multinet-plan.md) (management plan).
Versions referenced: `tailscale.com v1.104.2` (go.mod).

## TL;DR

| Feature | Effort | Why |
| --- | --- | --- |
| Subnet routes | moderate (days) | tsnet side is one pref; work is selection/config/UI plumbing plus overlap rules |
| Exit node | hard (a week+, mostly hardening) | needs a host default route through a TUN → loop avoidance, DNS, IPv6, fail-closed, crash cleanup |

Ship subnet routes first (phase 1). Exit node is phase 2, gated on a spike
(phase 2.0) that proves the loop-avoidance rule below on a real host.

## Why this doesn't fit today's design

Today everything is name-keyed: DNS hands out a synthetic `198.18.x` IP,
`registry.lookup(dstIP)` maps it back to a name, `forwarder.dialTailnet`
resolves the name via the tailnet peer list and `tsnet.Dial`s it. A
non-synthetic destination is a RST (`forwarder.handle`: "no name for synthetic
dst"). Kernel steering is one plain `ip route <cidr> dev <tun>` per tailnet; no
policy routing.

Subnet routes and exit nodes are **literal-IP** traffic, so both need a
second forwarding path: "dst is not synthetic but is inside a selected route →
dial the literal IP:port through this tailnet".

## Verified facts (tailscale.com v1.104.2)

1. **Accept-routes is OFF for tsnet on Linux.** `ipn.NewPrefs()` sets
   `RouteAll = DefaultRouteAll(GOOS)`, which is `false` on linux. tsnet uses
   `NewPrefs()` (`tsnet.go` ~L982). → must `EditPrefs{RouteAll: true}` per
   tailnet that has routes selected (same pattern as `setReportPosture` in
   `tailnet.go`).
2. **tsnet already marks its underlay sockets.** `net/netns/netns_linux.go`
   `controlC` sets `SO_MARK 0x80000` (`tsconst.LinuxBypassMarkNum`) on every
   non-localhost socket when running as root (falls back to `SO_BINDTODEVICE`
   if `SO_MARK` fails). Used by magicsock (WireGuard UDP), derphttp (DERP), and
   `tsdial.SystemDial` (control plane). The daemon runs as root
   (`install-ts-multinet.sh` requires root; unit has no `User=`).
   → Loop avoidance for an exit default route needs **zero tsnet changes**:
   one `ip rule fwmark 0x80000/0xff0000 lookup main`. This is the same rule
   tailscaled installs (pref 5210).
3. Exit-node usage works in userspace netstack: set `ExitNodeID` (or
   `ExitNodeIP`) via `EditPrefs`; `tsnet.Dial` to any internet IP then egresses
   via the exit peer.

## Phase 1 — subnet routes

### Behaviour
- A tailnet can select advertised subnet routes as resources: `route:<cidr>`
  (e.g. `route:10.20.0.0/16`). Optionally a sub-prefix of an advertised route.
- Selected route → kernel `ip route add <cidr> dev <tun>`; packets to it hit the
  gVisor stack; forwarder dials the **literal** dst IP:port via `tsnet.Dial`.
- TCP, UDP, ICMP echo, same as names.

### Work items
1. **Prefs**: `EditPrefs{RouteAll: true, RouteAllSet: true}` when the tailnet
   has ≥1 selected route; `false` otherwise (config owns consent, like
   posture). `tailnet.go`.
2. **Discovery**: list subnet routes from status — `PrimaryRoutes` on peers
   (what control actually assigned), excluding `0.0.0.0/0`/`::/0` (exit) and
   single-IP VIP entries (already special-cased in
   `selection.go:serviceHostTags`). Show in `peers` output, `/peers` API, web UI
   (new ROUTES column or section). Skip Mullvad peers as today.
3. **Selection**: new resource kind `route:<cidr>` in `resolveSelections`
   (`selection.go`), `select`/`forget`/`lock` validation ("must be covered by a
   live advertised route on <tailnet>"), auto_select rule `route:<cidr>` (exact
   or covered-by). `allow_all` does **not** imply routes (routes change host
   routing; must be explicit).
4. **Config**: `resources` already holds strings; `route:` prefix needs no
   schema change. configpatch untouched apart from tests.
5. **Registry**: `registry.routes map[tailnet][]netip.Prefix` +
   `routeFor(ip) (tailnet, ok)`. Rebuilt on every selection resolve.
6. **Forwarder fallthrough** (`forwarder.go handle`, `udp.go setupUDP`,
   `icmp.go handleICMPv4Echo`): if `lookup` misses and `routeFor(dst)` hits
   this tailnet → dial literal `dst:port`. Keep the synthetic-loop guard.
   Unknown dst → RST as today.
7. **Kernel routes**: `addRoute(cidr, dev)` / `delRoute` (exist in
   `tun_linux.go`) on selection change; diffed, not flushed. `cleanupTUNImpl`
   must also remove route prefixes (today it only knows the synthetic CIDR) —
   track them per tailnet.
8. **Overlap policy** (the real design decision):
   - Same prefix selected on two tailnets → **reject** the second selection
     with a clear error naming the owner. Kernel can't hold two equal routes
     to different devs meaningfully.
   - Nested prefixes across tailnets (10.0.0.0/8 on A, 10.1.0.0/16 on B) →
     allowed, longest-prefix wins (kernel does it). Show it in `status`.
   - Prefix overlapping a local interface subnet, the default gateway, or any
     synthetic `198.18.0.0/15` range → **reject** (loop / LAN breakage).
   - Out of scope: 1:1 NAT remapping of colliding subnets into synthetic space.
9. **DNS**: no change. Hosts inside the subnet are reached by IP, or by
   whatever DNS the user already has for them. (Optional later: forward a
   configured `route_dns` domain to a resolver inside the subnet.)
10. **Docs**: README + ts-multinet.md comparison table row ("subnet routes:
    yes, non-overlapping").

### Tests
- `resolveSelections` with route resources (covered, uncovered, withdrawn).
- Overlap validator (equal, nested, local-LAN, synthetic).
- Forwarder: literal-IP dial via route; miss → RST (`forwarder_net_test.go`
  style with fake dial).
- Route add/remove diffing with fake `addRoute`/`delRoute`.

## Phase 2 — exit node

### Behaviour
- One global setting: `exit_node: {"tailnet": "skynet", "peer": "exit-nyc"}`
  (top-level, not per tailnet — the host has one default route). CLI
  `ts-multinet exit <tailnet> <peer>` / `exit off`; web UI toggle.
- All non-local, non-tailnet traffic from the host egresses via that peer.
  Selected names and subnet routes on other tailnets keep working (more
  specific routes in `main` win).

### 2.0 Spike (go/no-go, half a day)
On a real host as root, with one tailnet and a known exit peer:
1. `EditPrefs{ExitNodeID: <peer>}` on the tsnet node.
2. Apply the routing below by hand; confirm `curl ifconfig.me` shows the exit
   IP, tailnet stays connected (WireGuard + DERP + control), no loop.
3. Confirm with `ss -e` / `conntrack` that tsnet sockets carry mark `0x80000`.
4. Pull the uplink / switch wifi; confirm recovery.
If the mark is absent (not root, `SO_MARK` failing → `SO_BINDTODEVICE` path),
fall back to the uidrange variant (below).

### Routing (policy routing, mark-based — no tsnet changes)
Table and prefs chosen to sit beside tailscaled's (52 / 52xx) without clashing.

```sh
T=5300  # ts-multinet exit table
ip route add default dev "$EXIT_TUN" table $T
ip -6 route add default dev "$EXIT_TUN" table $T          # or blackhole, see IPv6

ip rule add pref 5310 fwmark 0x80000/0xff0000 lookup main  # tsnet underlay → real uplink
ip rule add pref 5320 lookup main suppress_prefixlength 0  # LAN, synthetic, subnet routes win
ip rule add pref 5330 lookup $T                            # everything else → exit TUN
# same three for ip -6
```

`suppress_prefixlength 0` = "use main unless the match is main's default
route", so LAN, the synthetic ranges and phase-1 subnet routes are untouched.

Fallback variant if marks don't hold: run the daemon as a dedicated uid with
`AmbientCapabilities=CAP_NET_ADMIN` and replace pref 5310 with
`uidrange <uid>-<uid> lookup main`. Costs: polkit for resolved D-Bus, ACL/group
for `/etc/hosts`, installer changes. Only do this if the spike fails.

### Forwarder
Exit TUN forwarder: on `lookup` + `routeFor` miss, if this tailnet is the exit
tailnet → dial literal `dst:port` (TCP/UDP; ICMP echo via the existing ping
path or drop). Other tailnets keep RST-on-miss.

### DNS
- Non-tailnet queries go to `upstream_dns` from the daemon's own socket. That
  socket is unmarked (plain `net` dial, not netns) → it follows pref 5330 →
  exits via the TUN → forwarder dials it through the exit. So upstream DNS
  egresses via the exit by default. **Verify in the spike.**
- If `upstream_dns` is a LAN resolver, it's matched by pref 5320 and leaks
  queries locally. Option `exit_dns` (default: exit peer's DNS / 1.1.1.1)
  used while an exit is active.
- systemd-resolved: other links' DNS servers (DHCP-provided, on the LAN) still
  get queries for `~.` unless the exit TUN link claims the default route
  domain: `resolvectl domain <exit-tun> '~.'` + `resolvectl default-route
  <exit-tun> yes` while active; revert on off. `resolved.go`.

### IPv6
Synthetic space is v4-only and tsnet exit v6 depends on the exit peer. Default:
if the exit peer offers `::/0`, route v6 via the TUN; otherwise install
`blackhole default` in table 5300 v6 so v6 fails closed instead of leaking.
gVisor stack already has an IPv6 NIC route (`forwarder.go`).

### Failure and cleanup
- **Fail closed**: exit tailnet not Running / exit peer offline → keep the
  rules; table 5300 default stays on the TUN; forwarder RSTs. No silent
  fallback to the bare uplink. `status` and UI show "exit down".
- `exit off`, tailnet disable/remove, daemon shutdown → remove rules + table,
  revert resolved link, clear `ExitNodeID`.
- **Crash leftovers**: on startup, before anything else, delete rules at prefs
  5310–5330 and flush table 5300 (idempotent, owned range). Stale rules
  pointing at a vanished TUN would black-hole the host.
- Mullvad: exit selection allows Mullvad peers (they're filtered from name
  selection today; the exit use case is exactly them). Listing them under
  `exit` only, not `peers`.

### Coexistence
- Host tailscaled with its own exit node active → two default-route owners.
  Detect tailscaled's table 52 default route; refuse to enable with a clear
  error.
- tailscaled's pref 5210 rule uses the same mark → harmless overlap (both
  say "marked → main").
- Docker/libvirt bridges are in main → covered by pref 5320.

### Work items
1. Config: top-level `exit_node` (+ `exit_dns`); configpatch; live apply via
   `applyConfigUpdate` (not restart-only).
2. Prefs: `ExitNodeID` on the exit tailnet; clear on others.
3. `exit_linux.go`: rule/table install/remove/cleanup via `ip` (same `run()`
   helper as `tun_linux.go`), v4+v6.
4. Forwarder literal-IP path for the exit tailnet.
5. resolved default-route claim/revert.
6. CLI `exit`, `/exit` API, web UI, `status` line.
7. Startup crash cleanup.
8. Docs + comparison table row.

### Tests
- Rule/route command generation (golden argv, fake `run`).
- Startup cleanup is idempotent with/without leftovers.
- Config: one exit max, tailnet must exist and be enabled, peer must offer
  `0.0.0.0/0`.
- Fail-closed state machine (exit tailnet down → rules stay).
- Manual: spike checklist above, re-run on release.

## Known limits / non-goals
- One exit node per host (single default route).
- Colliding equal subnet routes across tailnets: rejected, not remapped.
- No per-app exit selection (would need cgroup marks — out of scope).
- Throughput: exit traffic is double userspace stack (gVisor TUN → splice →
  tsnet netstack). Fine for interactive/browsing; not for bulk transfer.
- No advertising routes or acting as an exit node (inbound) — still
  outbound-only.

## Alternative considered: per-tailnet SOCKS5/HTTP proxy for exit
No host routing at all: `tsnet.Dial` behind a local proxy listener, apps opt
in. Zero loop/DNS/IPv6/cleanup risk, but not transparent. Keep as an optional
add-on, not the exit-node implementation.
