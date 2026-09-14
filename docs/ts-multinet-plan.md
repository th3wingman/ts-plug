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

## Lane D — on-host acceptance (in progress)

Environment verified: resolved stub on 127.0.0.53:53 (so 127.0.0.1:53 free),
resolvectl present, tailscale0 per-link DNS pattern confirmed. Daemon running
current binary (md5-verified against build).

Blocked on: **completing the skynet login**. Node sits in NeedsLogin; auth
attempts so far died on expired/superseded URLs (each StartLoginInteractive
invalidates the previous session — fixed in 5a07247, needs reinstall).
Next: reinstall, click Login once, auth the URL immediately. If auth completes
but state doesn't flip: check admin console → Machines for
`ts-multinet-skynet` (device approval), and what the browser page reported.

Then, per the acceptance checklist:

1. `resolvectl query <peer>.skynet` and `<peer>.tailXXXX.ts.net` → same
   synthetic IP; `ping <peer>.skynet` works through system DNS
2. `resolvectl status <tun>` shows our DNS + routing domains; other links
   (incl. tailscale0 when running) untouched
3. select/forget round-trip via CLI and UI; config comments survive
4. `allow-all` on/off; domain set/clear
5. add/remove a tailnet live; re-add without browser login
6. second tailnet (e.g. `dev`) end-to-end
7. hosts block consistent with DNS answers

Note: regular `tailscaled` was stopped on this host during testing (clean
system); it coexists by design — restart it any time.

## Backlog (not in scope of this plan)

- original roadmap items remaining: macOS/Windows TUN backends, literal `100.x`
  access, throughput work, IPv6 synthetic ranges ([ts-multinet.md](ts-multinet.md))
- bare short names (`my-server`) system-wide (search-domain decisions)
- `ui_listen` disable sentinel (currently empty string falls back to default)
- container resolv.conf search list updates on domain change (startup-only now)
- `hostname` in the UI add-form / `add` CLI positional (config-edit + reload
  works today)
- peers port-probe polling may be chatty on large tailnets
- PR from `ts-plug/multinet-host-mode` when Lane D passes
