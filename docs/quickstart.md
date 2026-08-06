# Quick Start

Copy-paste recipes for the common cases. Everything here installs a systemd
service via [`scripts/install-systemd.sh`](../scripts/install-systemd.sh) —
each instance is an isolated tailnet node with its own state under
`/var/lib/<tool>/<name>/`.

## Prerequisites

- A Tailscale auth key: https://login.tailscale.com/admin/settings/keys
  (a plain one-off `tskey-auth-...` is fine)
- Linux with systemd. No Go, no clone needed — binaries come from
  [GitHub releases](https://github.com/th3wingman/ts-plug/releases)
  (amd64, arm64, armv7/Pi)

The one-liner used throughout (piped stdin disables the hidden key prompt, so
the key goes on the command line; from a clone you can run the script directly
and get prompted instead):

```sh
curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
  | sudo bash -s -- <tool> <flags...> --authkey tskey-auth-...
```

## Expose SSH to your tailnet

```sh
curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
  | sudo bash -s -- ts-plug --name mybox-ssh --port 22 --authkey tskey-auth-...

# from any tailnet device:
ssh user@mybox-ssh.<your-tailnet>.ts.net
```

### Variant: zero open TCP ports (systemd ≥ 256)

sshd stays reachable through its unix socket only; nothing listens on LAN :22:

```sh
sudo systemctl disable --now ssh.service ssh.socket
systemctl status sshd-unix-local.socket          # must be active

curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
  | sudo bash -s -- ts-plug --name mybox-ssh \
      --src-port 22 --dst-socket /run/ssh-unix-local/socket --authkey tskey-auth-...
```

## Expose a local web app over HTTPS

```sh
# https://grafana.<your-tailnet>.ts.net -> 127.0.0.1:3000, TLS cert handled for you
curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
  | sudo bash -s -- ts-plug --name grafana --proto https --dst-port 3000 --authkey tskey-auth-...
```

Callers arrive with `Tailscale-User-*` identity headers — see the
[ts-plug guide](./ts-plug.md) for header mapping and `-public` (Funnel).

## Bring a remote service to localhost

```sh
# 127.0.0.1:5432 -> postgres on another tailnet host
curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
  | sudo bash -s -- ts-unplug --name db --port 5432 --mode tcp db.tailnet.ts.net:5432 --authkey tskey-auth-...

psql -h 127.0.0.1 -p 5432
```

## Remote docker socket, mounted locally

Two halves — ts-plug on the docker host, ts-unplug on your machine:

```sh
# on the docker host
curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
  | sudo bash -s -- ts-plug --name dockerbox \
      --src-port 2375 --dst-socket /var/run/docker.sock --group docker --authkey tskey-auth-...

# on your laptop
curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
  | sudo bash -s -- ts-unplug --name rdocker --mode tcp \
      --src-socket /run/ts-unplug/rdocker/docker.sock dockerbox:2375 --authkey tskey-auth-...

export DOCKER_HOST=unix:///run/ts-unplug/rdocker/docker.sock
docker ps
```

> **Warning:** the docker socket is root on that host, and this exposes it to
> **every device on your tailnet**. Restrict access with
> [Tailscale ACLs](https://tailscale.com/kb/1018/acls). Never use `-public` here.

## Day-2 operations

```sh
journalctl -fu ts-plug@mybox-ssh.service        # logs / watch it join
systemctl status ts-plug@mybox-ssh              # status

# the auth key is only needed for the first join; after that you can remove it
sudoedit /etc/ts-plug/mybox-ssh.env             # delete the TS_AUTHKEY= line

# uninstall (keeps node identity for painless reinstall)
sudo scripts/install-systemd.sh ts-plug --name mybox-ssh --uninstall
# uninstall + delete node keys (also remove the device in the admin console)
sudo scripts/install-systemd.sh ts-plug --name mybox-ssh --uninstall --purge
```

Instances stack: `ts-plug@ssh`, `ts-plug@grafana`, `ts-unplug@db` coexist,
each with its own tailnet identity.

## Where things live

| What | Path |
|---|---|
| binary | `/usr/local/bin/<tool>` |
| unit template | `/etc/systemd/system/<tool>@.service` |
| per-instance config (auth key + args) | `/etc/<tool>/<name>.env` (0600) |
| node keys, certs, tsnet state | `/var/lib/<tool>/<name>/` |
| unix sockets served by ts-unplug | `/run/ts-unplug/<name>/` |

## Troubleshooting

- **Service starts but node never appears** — auth key missing/expired: check
  `journalctl -u <tool>@<name>` and `/etc/<tool>/<name>.env`.
- **Hostname collision** — pick a `--name` that isn't the machine's existing
  tailscaled hostname (that's why examples use `mybox-ssh`, not `mybox`).
- **Wrong arch downloaded** — 64-bit kernel with 32-bit userland (some Pis):
  pass `--arch armv7`.
- **Can't reach a group-owned socket** — add `--group <group>` (e.g. `docker`)
  so the DynamicUser service joins it.

More depth: [ts-plug](./ts-plug.md) · [ts-unplug](./ts-unplug.md) ·
[ts-router](./ts-router.md) · [use cases](./use-cases.md)
