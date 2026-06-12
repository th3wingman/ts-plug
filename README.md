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
