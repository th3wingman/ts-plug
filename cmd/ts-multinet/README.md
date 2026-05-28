# ts-multinet

Run several tailnets transparently on one host at the same time.

`tailscaled` gives the whole OS one tailnet. `ts-multinet` gives it *several* —
no `ip rule`, no `iptables`, no profile switching. Each tailnet is a stock
userspace **tsnet** node; in front of each we run a small gVisor TCP/IP stack on
its own TUN device (tun2socks style) and re-dial every connection out through
that tailnet.

> **Status: MVP / RnD.** Linux only, TCP only, meant to run inside a container
> network namespace. macOS/Windows and UDP/ICMP are future work.

## How it works

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

`config.example.json`:

```json
{
  "mtu": 1280,
  "dns_listen": "127.0.0.1:53",
  "tailnets": [
    {"name": "skynet",   "suffix": "skynet.ts.net",   "authkey_env": "TS_AUTHKEY_SKYNET",   "cidr": "198.18.1.0/24", "tun": "tsm-skynet"},
    {"name": "othernet", "suffix": "othernet.ts.net", "authkey_env": "TS_AUTHKEY_OTHERNET", "cidr": "198.18.2.0/24", "tun": "tsm-othernet"}
  ]
}
```

- **Auth keys are read from env**, not the file. Use reusable or ephemeral keys.
- `tun` names must be ≤15 chars (kernel `IFNAMSIZ`).
- Non-tailnet DNS is forwarded to the upstream inherited from the original
  `/etc/resolv.conf` (override with `"upstream_dns"`).

## Run (container)

```sh
# from the repo root
make docker-ts-multinet

docker run --rm -it \
  --cap-add NET_ADMIN --device /dev/net/tun \
  -e TS_AUTHKEY_SKYNET=tskey-auth-... \
  -e TS_AUTHKEY_OTHERNET=tskey-auth-... \
  ts-multinet
```

Then, in another shell, exercise both tailnets transparently:

```sh
docker exec -it <container> curl -sk https://<host>.skynet.ts.net/
docker exec -it <container> curl -sk https://<host>.othernet.ts.net/
```

Both resolve and connect at the same time, each over its own tailnet, with an
unmodified `curl`.

## Discovering hosts & diagnosing

You don't have to guess what's on your tailnets. These run standalone (no TUNs,
no caps) — use a throwaway container so they don't fight the daemon for state:

```sh
# What hosts exist, and what do they actually serve? (probes :22,:80,:443,:8080)
docker run --rm -e TS_AUTHKEY_SKYNET -e TS_AUTHKEY_TSJUSTWORKS -e TS_AUTHKEY_BORDER0_COM \
  ts-multinet peers                 # everything (pass a filter on big tailnets)
docker run --rm ... ts-multinet peers rpi4            # name filter
docker run --rm ... ts-multinet -ports 22,5432,3000 peers db   # custom ports
```
```
== skynet (tail523555.ts.net) — 2 shown, 2 up ==
  STATE NAME            IP              OS     SERVICES
  UP    rpi4-sk-01      100.82.224.14   linux  :22
  UP    rpi4-st-gw-01   100.72.240.52   linux  :22
```

```sh
# Why did that connection hang/fail? Walk the whole path for one target:
docker run --rm ... ts-multinet check rpi4-sk-01.tail523555.ts.net:22
```
```
host:      rpi4-sk-01.tail523555.ts.net
tailnet:   skynet (tail523555.ts.net)
resolve:   rpi4-sk-01.tail523555.ts.net -> 100.82.224.14
dial:      100.82.224.14:22 over skynet ...
result:    OPEN (230ms) — banner: SSH-2.0-OpenSSH_10.2p1 Ubuntu-2ubuntu3.2
```

`check` tells you which step broke: `resolve FAILED` (not a peer), `UNREACHABLE`
(refused/timeout, with the reason), or `OPEN` with the latency — so a slow path
reads as `OPEN (7.2s)`, not a mystery hang.

## Limitations (MVP)

- **TCP only.** UDP and ICMP packets are dropped. (DNS works because the
  responder is a real UDP listener, not via the TUN.)
- **Name-based only.** Connecting to a literal `100.x` tailnet IP isn't steered
  — that's the overlapping-CGNAT case the synthetic ranges exist to avoid.
- **Container netns assumed.** Running on the host would rewrite the host's
  `/etc/resolv.conf` and add host routes. Use `-set-resolv=false` and wire DNS
  yourself if you try that.
