> [!WARNING]
> Lots of Work in Progress stuff here!

# What's in this repo?

One-liner tools to expose things to/from your tailnet!

| Binary | Purpose | Use Case |
|--------|---------|----------|
| **ts-plug** | Expose localhost to your tailnet | Share your dev server to your tailnet, deploy without sidecars |
| **ts-unplug** | Bring tailnet services to localhost | Access tailnet-based databases/APIs as if they were local |
| **ts-router** | Bring *many* tailnet hosts to localhost under their real URLs | Type `https://anything.skynet.ts.net/` in the browser and have it just work |
| **ts-multinet** *(RnD)* | Several tailnets transparently on one host at once | Reach services across many tailnets simultaneously — the thing `tailscaled` can't do. See [docs](./docs/ts-multinet.md) |

## Quick Start

**Build:**
```sh
make                    # Build both binaries
make install            # Install to $GOPATH/bin
```

**ts-plug** - Share a local service:
```sh
./build/ts-plug -hostname myapp -- python -m http.server 8080
# Access at https://myapp.tailnet-name.ts.net
```

**ts-unplug** - Access a remote service:
```sh
./build/ts-unplug -dir ./state -port 8080 api.tailnet-name.ts.net
# Access at http://localhost:8080
```

**ts-router** - Bring many tailnet hosts to localhost under their real URLs:
```sh
mkdir -p ~/.config/ts-router/skynet
$EDITOR ~/.config/ts-router/skynet/routes.json
./build/ts-router -instance ~/.config/ts-router/skynet -hostname tsrouter-skynet-$(hostname -s)
# Browse https://ai.skynet.ts.net/, https://app.skynet.ts.net/, etc.
```

## Run as a Service

One-liner systemd install for any of the tools — sets up an isolated instance under `/var/lib/<tool>/<name>`. No clone, no Go toolchain: the script pulls a checksum-verified binary from [GitHub releases](https://github.com/th3wingman/ts-plug/releases):

```sh
# Expose local sshd at my-host-ssh.<tailnet>.ts.net:22 — works on amd64, arm64, Pi (armv7)
curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
  | sudo bash -s -- ts-plug --name my-host-ssh --port 22 --authkey tskey-auth-...
```

From a clone the same script builds locally instead (and prompts for the auth key, so it never touches your shell history):

```sh
# Expose local sshd at my-laptop-ssh.<tailnet>.ts.net:22
sudo scripts/install-systemd.sh ts-plug --name my-laptop-ssh --port 22

# Expose local Grafana at https://grafana.<tailnet>.ts.net
sudo scripts/install-systemd.sh ts-plug --name grafana --proto https --dst-port 3000

# Bring tailnet postgres to 127.0.0.1:5432
sudo scripts/install-systemd.sh ts-unplug --name db --port 5432 --mode tcp db.tailnet.ts.net:5432

# Unix sockets work on both ends: tailnet-only ssh (zero open TCP ports, systemd 256+),
# or a remote docker socket mounted locally — see docs/ts-plug.md and docs/ts-unplug.md
sudo scripts/install-systemd.sh ts-plug --name xps13-ssh --src-port 22 --dst-socket /run/ssh-unix-local/socket

# Remote install (Raspberry Pi etc.): cross-compile, ship, install over SSH
make deploy HOST=pi.local TS_AUTHKEY=tskey-auth-...
```

Instances stack: `ts-plug@ssh`, `ts-plug@grafana`, ... each with its own tailnet identity. `--uninstall` removes one, `--help` shows everything else. More recipes in the [Quick Start Guide](./docs/quickstart.md).

## Key Features

**ts-plug** automatically:
- Starts your upstream server
- Joins your tailnet with TLS and DNS
- Reverse proxies to localhost:8080
- Optional public access with `-public`
- Supports HTTP, HTTPS, and DNS protocols
- Injects the caller's tailnet identity as `Tailscale-User-Login/-Name/-Profile-Pic` headers, overwritten on every request so clients can't spoof them

**Identity header mapping** — apps that expect a different auth-proxy dialect work unmodified:
```sh
# Map identity fields to custom header names (login, name, pic)
ts-plug -hn myapp -header-map login=X-Auth-Email,name=X-Auth-Name -- ./myapp

# Border0 preset: X-Auth-Email, X-Auth-Name, X-Auth-Picture
ts-plug -hn myapp -border0 -https-port 443:8181 -- ./myapp
```
Mapped headers get the same overwrite-always treatment as the `Tailscale-User-*` set. With `-public`, public visitors arrive with blank identity headers — make sure the upstream has a fallback auth tier. Slow upstreams (e.g. LLM apps) can raise `-upstream-timeout` (default 30s) to allow longer time-to-first-byte.

**ts-unplug** provides:
- Reverse proxy from tailnet to localhost
- Access to services requiring localhost URLs
- Simple port mapping

## Examples

Run servers in any language:
```sh
make examples

# Try different languages with ts-plug
./build/ts-plug -hn hello -- ./build/hello        # Go
./build/ts-plug -hn hello -- cmd/examples/hello/hello.js   # Node
./build/ts-plug -hn hello -- cmd/examples/hello/hello.py   # Python
```

See [cmd/examples/](./cmd/examples/) for more.

## Docker Integration

Use ts-plug as an entrypoint to eliminate sidecar containers:

```dockerfile
COPY ts-plug /usr/local/bin/
ENTRYPOINT ["ts-plug", "-hostname", "myapp", "--"]
CMD ["npm", "start"]
```

See [docker/](./docker/) for Pi-hole, Open WebUI, and Audiobookshelf examples.

## Documentation

- **[Quick Start Guide](./docs/quickstart.md)** - Copy-paste recipes: ssh, https apps, remote docker socket, day-2 ops
- **[Complete Documentation](./docs/)** - Guides, use cases, and detailed examples
- **[ts-plug Guide](./docs/ts-plug.md)** - Full ts-plug documentation
- **[ts-unplug Guide](./docs/ts-unplug.md)** - Full ts-unplug documentation
- **[ts-router Guide](./docs/ts-router.md)** - Full ts-router documentation
- **[Use Cases](./docs/use-cases.md)** - Real-world scenarios
- **[Docker Guide](./docs/docker.md)** - Container integration

**Quick help:**
```sh
./build/ts-plug -h
./build/ts-unplug -h
./build/ts-router -h
```

## License

BSD-3-Clause - See [LICENSE](./LICENSE)
