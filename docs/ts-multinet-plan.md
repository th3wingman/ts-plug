# ts-multinet management plan — status

Working record of the approved plan (host DNS, management CLI, web UI) and its
mid-flight extension (runtime tailnet lifecycle). Implementation branch:
`ts-plug/multinet-host-mode`. Companion engineering notes: [ts-multinet.md](ts-multinet.md).

## Original goals (approved plan)

1. Host DNS service so the daemon doesn't rely on the /etc/hosts block (side by side, not instead)
2. CLI controls instead of config-file-only management
3. Web UI in the Cauldron pattern (browser configure/manage/control)
4. Expose all resources or select them
5. DNS custom domains: `my-server.tailXXXX.ts.net` and `my-server.skynet` both resolve
6. Updated install scripts and docs

Extension (added during implementation): default install ships with nothing
configured; tailnets are added/removed live via CLI/UI; config re-read without
systemd restarts; daemon owns the config file but manual edits survive.

## Implemented

| Area | What | Commit |
| --- | --- | --- |
| Config format | HuJSON loader (comments + trailing commas), all-comments example | `9afe174` |
| Lane A | comment-preserving config patching (`configpatch.go`), `applyConfigUpdate` single mutation path, endpoints select/forget/allow-all/domain + GET /config | `7eca17c` |
| Lane B1 | per-TUN systemd-resolved registration (`resolved.go`), custom-domain DNS matching (alias suffix parity, longest match), port-53 bind-conflict degrade (hosts-block-only) | `713d3e3` |
| Lane B2 | CLI verbs `select`/`forget`/`allow-all`/`domain`/`config` | `4641342` |
| Lane B3 | embedded web UI (vanilla JS, no framework) on `ui_listen` (default 127.0.0.1:8123), localhost-only, no auth | `2590357` |
| Lane C | README/docs/install script/example config for all of the above | `8860e28` |
| Lane E1 | runtime lifecycle: `syncTailnets` diff (stop/swap/start), per-tailnet cancel, registry scrub, per-TUN resolved revert, zero-tailnet configs, `reload` = re-read + sync | `622f3bc` |
| Lane E2 | `add`/`remove` CLI verbs + UI form (cidr/tun auto-picked), empty-by-default install, UI empty state | `0ef84d8` |
| Fixes | login URL fallback (watcher-recorded) + save-button CSS squeeze | `9ffad37` |
| Fixes | per-tailnet `hostname` config key (default `ts-multinet-<name>`), live-applied | `c10272a` |
| Fixes | login returns pending URL instead of invalidating the session | `5a07247` |
| Root cause | **tailscale.com v1.94.2 → v1.102.3** — the old tsnet never consumed a completed browser auth (bisected with a standalone probe; cauldron's version works) | `e30e443` |
| Naming | free-form tailnet names (path-safe, any printable) + DNS-safe `slugify`; `domain` pre-populated with the slug; `hostname` as first-class control (verb + UI editor + add-form field) | `165a9e7` |
| CLI | readable non-2xx errors (plain-text 404 no longer leaks json garbage); install script **restarts** a running daemon (`enable --now` never did) | `369e569` |
| Races | `Close` waits for forwarder/watcher goroutines (stop/start TUNSETIFF EBUSY); `handlePeers` snapshots the tailnet list under the mutex (nil-panic) | `41009ac` |
| Races | starts retry briefly-held TUN names (systemd restart overlap with the dying daemon) | `5cae8ac` |
| Lifecycle | hostname kept on `add` (insert builder fix); stop cleans the tun route/addr before close; busy starts self-heal in the background (5–45s backoff); config-only mutations work on parked tailnets; sync start failures are a notice, not a 500 | `dc5b363` |
| UX | parked tailnets (in config, not running) get "`reload` retries it" instead of "no such tailnet" | `110ad77` |
| UX | short single-label domains (≤4 chars: dev, io, app…) warned at add, domain set, and start — they collide with public TLD space | `66041a4` |
| UI | drill-down console: hash router, read-only Overview, per-tailnet detail (Peers filter/cap + click-only probing; Settings), Config page for globals + add; API additions `dns_registered` + `POST /config` (globals, comment-preserving, needs_restart) | `bd4d245`, `6627807` |
| UI | structural **cidr/tun** endpoints (range/overlap + device-clash validation, works parked, applies via stop/start) wired into Settings; async TUN-retry nil-ctx panic fixed (found by the new tests) | `e718d69` |
| Services | advertised `svc:<label>` VIP services as first-class selectable resources: discovery via `lc.GetServices`, service-aware resolver + `resolveSelections`, `GET /services`, `svc:` select validation naming the kind, hosts-block alias parity, `services` CLI verb + `--details status` breakdown, UI Services sub-tab | *this branch* |
| UI polish | config-drift indicator (`GET /applied` + topbar applied/dirty), `POST /tailnet/{name}/clear`, overview summary line + services count, detail breadcrumb, Peers **hide inactive**, **clear all** on Peers/Services | *this branch* |
| Resilience | self-heal watchdog restarts a tailnet left dark by a network outage (~90s without a control-plane poll, or a dead datapath), `POST /tailnet/{name}/restart` + CLI `restart` + UI button, forwarder pumps surface fatal errors instead of living on half-dead, resolved per-link config re-applied every 60s (survives a systemd-resolved restart) | *this branch* |
| Docs | plan status updates | `a837866` |
| CLI | `--json` / `--details` flags on every command (raw replies, JSONL multi-peer, extended human forms); peers replies carry FQDN | `872c1c3` |
| DNS hardening | routing domains set before the DNS server at registration; forwarder prefers real upstreams (`/run/systemd/resolve/resolv.conf`) over the resolved stub — no forwarding loop | `a593d09` |
| DNS fix | friendly-alias misses (e.g. `pi.dev` — `dev` is a real gTLD) fall through to the public upstream; only MagicDNS-suffix misses stay NXDOMAIN | `1762455` |

All unit gates green per commit: `go build ./...`, `go vet`, `go test ./cmd/ts-multinet/ -count=1`.

## Locked design decisions

- **DNS**: systemd-resolved per-TUN routing domains (`~<suffix>` + `~<domain>`),
  tailscaled's pattern; coexists with other per-link DNS. No resolved / port 53
  held → warn + /etc/hosts block only (never fatal on host; fatal in containers).
  The hosts block is kept as fallback, not removed.
- **Config ownership**: the daemon patches the file in place through the hujson
  AST — comments survive every write; manual edits stay supported (`reload`
  applies them). Only globals (`mtu`, `dns_listen`, `upstream_dns`, `state_dir`,
  `hosts_file`, `ui_listen`) need a service restart.
- **Web UI**: localhost-only, no auth (same trust as the root unix control
  socket); remote management = SSH tunnel.
- **Domains**: `domain` defaults to the tailnet name; both spellings resolve to
  the same synthetic IP.
- **Removal**: node state (login identity, pins) is kept; re-add logs back in
  without a browser.
- **Renames** are structurally remove+add (same net effect).
- **Start failures** stay in config and are retried by any later sync/reload
  (self-healing).

## Deviations from the approved plan

- Tailnet add/remove moved from "manual edit + restart" (original plan) to
  fully runtime (E1/E2) — user request during Lane D.
- Structural config patching (add/remove/rewrite tailnet elements) was added;
  the original plan's configpatch only handled mutable fields.
- `POST /tailnet` + `DELETE /tailnet/{name}` endpoints were not in the original
  API sketch.
- Fixed en route: registry domain sync on domain edits (latent bug, B1/E1);
  `confFor` parsing comments (Lane A); login session invalidation (5a07247).

## Lane D — on-host acceptance (nearly done)

Root cause of the login saga found and fixed by bisect: `tailscale.com
v1.94.2` never consumed a completed browser auth; a standalone probe on
`v1.102.3` flipped to Running seconds after auth. Bumped in `e30e443`.

Verified live on this host (three tailnets, real logins):

1. ✅ skynet Running, hostname `xps13-m` (custom), 11 selections; `nucbox.skynet`
   and `nucbox.tail95e9d1.ts.net` → same synthetic IP; `ping` 2/2 through the
   TUN; hosts block consistent
2. ✅ msinfra (66 peers) + dev Running side by side, per-TUN resolved
   registrations (`~tail84a2fd.ts.net ~msinfra` on tsm1, `~tail30fc5e.ts.net
   ~dev` on tsm2), disjoint synthetic ranges
3. ✅ free-form names, domain pre-population, hostname control (verb + UI)
4. ✅ restart-overlap EBUSY fixed twice over: Close waits for goroutines
   (`41009ac`) and starts retry briefly-held TUN names (`5cae8ac`); peers
   snapshot under the mutex (nil-panic fix, same commit)
5. ✅ CLI `--json` / `--details` flags (status/peers/check/config + mutations);
   peers replies carry FQDN

## In flight

Nothing — the CLI flags lane (`872c1c3`) and both DNS hardening pieces
(`a593d09`) are merged.

## Pick up here (next session)

0. **Scale lane, shape depends on the user's answer (ASK: handful vs bulk)**:
   corp has 1834 machines, ACL-visible to this node: 42 (+ 536 Mullvad
   transit, filtered everywhere). (a) peers view hardening (cap/filter/no
   auto-probe) only matters if ACLs expose big sets; (b) bulk (allow-all)
   would need wider synthetic cidrs (/24 caps at 254).
1. ✅ **Reinstall + verify done (post-b787d0f)**: pi.dev resolves, corp shows
   33/42 real peers, all four tailnets Running, --details clean, restart
   without DNS/EBUSY incidents.
2. User housekeeping (not code): `sudo resolvectl revert` clears the three
   stale **global** `DNS Servers: 127.0.0.1` entries left by an early build;
   delete the `tsm-probe` device from the SkyNet console (bisect artifact);
   check corp's ACLs for the 42-of-1834 visibility; regular `tailscaled`
   restart whenever (stopped here for a clean system — coexists by design).
3. ✅ **PR open in the fork** from `ts-plug/multinet-host-mode` — fork-internal
   only (`origin`, not `upstream`); head → the fork's default branch. Manual
   verification pending.
4. ✅ **Web UI redesign** implemented (`bd4d245`, `6627807`, `e718d69`) and
   ✅ **advertised services** implemented (this branch, design record below);
   remaining: the on-host walkthrough with the user (Overview, drill-down,
   probe-on-demand via journal, **Services** tab, Settings saves, Config
   restart badges) and the loopback `POST /config` + `cidr/tun` smoke. One
   reinstall covers both.

## Web UI redesign — implemented

Lanes: A daemon (`bd4d245`), B UI (`6627807`), C wrap-up (`e718d69`). Design
record below; deviations from the plan are noted at the end.

**Deviations/notes**: `enabled` toggle still missing — no endpoint exists for
toggling a tailnet's enabled flag, so it renders nowhere (would be a small
`updateTailnet` addition); cidr/tun became editable (`e718d69`) after lane B
correctly refused to invent endpoints; probe results are cached per poll cycle
and the 5s poll never sends `ports=`; `views/shared.js` holds state+actions to
keep the module graph acyclic (app → views → shared → api).

### Original design record

Approved design (plan-mode session, all three forks answered: drill-down IA,
filter+cap peers, vanilla restructured). Implementation starts from commit
`540e6fe`.

**IA**: hash routing (`#/`, `#/tailnet/<name>[/settings]`, `#/config`).
Overview is read-only: daemon header line, one summary row per tailnet
(state badge, name, suffix, domain, hostname, peers up/total, selected, DNS
registered dot), rows link to detail, zero actions; empty state CTA → Config.
Tailnet detail: header (name/state/hostname/our IP, Login button when not
Running) + sub-tabs **Peers** (search filter, table name/fqdn/IP/OS/online/
services, render cap 200 with "show all N", selection checkboxes, allow-all
rows checked+disabled; port probing click-only — the 5s poll never probes)
and **Settings** (domain, hostname with live-apply note, allow-all, enabled,
resources chips with per-chip forget, cidr/tun with stop/start note, suffix
read-only; danger zone: remove). Config page: globals form (mtu, dns_listen,
upstream_dns, ui_listen, hosts_file — restart badges; `state_dir` excluded,
orphas node state) + add-tailnet form + collapsible read-only effective-config
preview.

**API contract** (so lanes parallelize): `GET /status` per-tailnet gains
`dns_registered` bool from resolvedSync; new `POST /config` accepts the globals
subset, validates, patches the file via configpatch (comments survive),
returns `{ok, needs_restart: "mtu, dns_listen"}`; `state_dir` rejected with a
manual-edit pointer; `/peers` unchanged (UI stops passing `ports=` by
default).

**Code structure**: `index.html` shell only; `app.js` = state, 5s polling
(paused when hidden), router, shared fetch; `web/views/` = `overview.js`,
`tailnet.js`, `config.js`, `shared.js` (h(), badges, form rows — moved out of
app.js); `style.css` extended, same dark tokens.

**Lanes**: A (daemon: two API additions + tests), B (UI restructure, parallel
— the API contract is its interface), C (parent: review, merge, gates,
README/install touch-ups, on-host walkthrough).

**Acceptance**: Go unit tests per lane + `web_test.go` serving checks; manual
on this host — overview informative, skynet drill-down filter/cap/select,
settings saves live, cidr edit applies via stop/start without re-login, config
page shows restart badges, zero probe traffic during polls (journal), corp's
42-peer table instant, hash URLs survive reload/back.

**Assumptions**: hash routing; sub-tabs in detail; click-to-probe; no
virtualization/pagination/framework/auth changes; old dashboard inline editors
removed; probe list stays 22,80,443,8080.

## Advertised services (`svc:<label>`) — implemented

Tailscale advertised services (`svc:<label>` Service VIPs) are first-class
resources at parity with peer hosts, using the same config file, verbs and
UI/CLI surface. Grounded on `client/local` (tailscale.com v1.102.3):
`lc.GetServices(ctx) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error)`
exists on the same `*local.Client` each tailnet already holds — no new
dependency. `ServiceDetails{Name "svc:<label>", DisplayName, Addrs, Ports}`.

**Behaviour**

- Selected services live in the existing per-tailnet `resources` list as
  `svc:<label>` — same verbs, same comment-preserving config file.
- Discovery via `lc.GetServices`; the tailnet resolver (`newTailnetResolver`)
  checks peers then services, so `<label>.<suffix>` and `<label>.<domain>` map
  to one synthetic IP keyed on the canonical service FQDN (peers and services
  share the allocator, never the same key). Alias misses still fall through to
  the public upstream; MagicDNS-suffix misses stay authoritative NXDOMAIN.
- Forwarding unchanged: the forwarder dials the resolved target through
  `ts.Dial` with the client's port (TCP/UDP); ICMP to a service's synthetic IP
  answers unreachable via the existing TSMP ping path.
- Selected services land in the `/etc/hosts` block under both spellings, like
  peers (`resourceAlias` strips the `svc:` prefix for the alias).
- No port probing for services: advertised `Ports` are displayed as metadata.
- `GET /services` (per tailnet: `svc:` name, display name, VIPs, advertised
  ports, selected flag). `/peers` untouched. `select`/`forget` accept `svc:`
  names; `select` validates against the live service list and the error text
  names the kind. `forget` accepts any name (stale cleanup).
- CLI: `ts-multinet services [tailnet]` (`--json` / `--details`),
  `select`/`forget` accept `svc:<label>`, `--details status` carries an
  advertised/selected services count.
- UI: third sub-tab **Peers | Services | Settings**; Services table (name,
  display name, VIPs, advertised ports, selection checkbox), filter box, empty
  state explaining ACL-gated visibility, no probe button.

**Identity**: a service's `svc:<label>` name is its identity — services are not
node-key-pinned, so a vanished service surfaces like a missing peer, loudly.
`allow_all` still means every non-Mullvad **peer**; service selection is
per-service checkboxes.

**Assumptions**: any advertised port is forwardable (tailnet ACLs enforce);
services support TCP+UDP, ICMP unreachable; VIPs come from the tailnet address
space and route via `ts.Dial`/netstack; no new dependencies, no `/peers`
change, no auth change.

## Backlog (not in scope of this plan)

- original roadmap items remaining: macOS/Windows TUN backends, literal `100.x`
  access, throughput work, IPv6 synthetic ranges ([ts-multinet.md](ts-multinet.md))
- bare short names (`my-server`) system-wide (search-domain decisions)
- `ui_listen` disable sentinel (currently empty string falls back to default)
- container resolv.conf search list updates on domain change (startup-only now)
- peers port-probe polling may be chatty on large tailnets
- PR from `ts-plug/multinet-host-mode` when Lane D passes
