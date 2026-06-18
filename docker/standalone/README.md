# Standalone ts-plug image

`ts-plug` as its own container instead of baked into an app image. Use it as a
**sidecar** that shares another container's network namespace and exposes that
container's localhost port onto the tailnet.

```
docker build -f docker/standalone/Dockerfile -t ts-plug .
```

Published by CI to `ghcr.io/th3wingman/ts-plug` (`:latest` + the version tag) on
tag pushes (`v*`).

## Sidecar usage (docker compose)

```yaml
services:
  app:
    # ... your app, listening on :8181 ...
  ts-plug:
    image: ghcr.io/th3wingman/ts-plug:latest
    network_mode: "service:app"     # shares app's netns => localhost:8181 IS the app
    environment:
      - TS_AUTHKEY=${TS_AUTHKEY}     # rotate out after first auth; identity persists in /state
    volumes:
      - ts-plug-state:/state
    command:
      ["-hostname", "myapp", "-border0", "-https-port", "443:8181",
       "-dir", "/state", "--", "/bin/sleep", "infinity"]
volumes:
  ts-plug-state:
```

tsnet is userspace WireGuard: **no `/dev/net/tun`, no `NET_ADMIN`**. The base is
Debian (not Alpine) because the keepalive `-- /bin/sleep infinity` needs GNU
coreutils `sleep`.

> Requires the `-border0` / `-header-map` / `-upstream-timeout` flags. Build from a
> ref that includes them (the `ts-plug/header-map` work), not a bare `main` that
> predates it.
