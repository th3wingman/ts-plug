# ts-plug Guide

**ts-plug** wraps your local server and exposes it on your tailnet with automatic TLS and DNS.

## Overview

ts-plug is a reverse proxy that:
- Starts and manages your upstream server process
- Connects to your tailnet
- Provides automatic HTTPS with valid TLS certificates
- Optionally exposes services publicly via Tailscale Funnel
- Supports HTTP, HTTPS, and DNS proxying

## Installation

Build from source:
```sh
make ts-plug
```

Install to $GOPATH/bin:
```sh
make install
```

## Basic Usage

The basic pattern is:
```sh
ts-plug [flags] -- [your-server-command]
```

Everything after `--` is treated as the command to run.

### Simple Examples

Run a Python HTTP server on your tailnet:
```sh
ts-plug -hostname myserver -- python -m http.server 8080
```

Run a Node.js app:
```sh
ts-plug -hostname api -- node server.js
```

Run a Go server:
```sh
ts-plug -hostname webapp -- go run main.go
```

## How It Works

```
┌─────────────────────────────────────────────────────────┐
│  Your Local Machine                                     │
│                                                         │
│  ┌──────────────┐          ┌──────────────┐             │
│  │  ts-plug     │  starts  │ Your Server  │             │
│  │              │ ──────>  │ localhost:80 │             │
│  └──────┬───────┘          └──────────────┘             │
│         │                                               │
└─────────┼───────────────────────────────────────────────┘
          │ Tailscale (encrypted)
          │
┌─────────┼───────────────────────────────────────────────┐
│  Your tailnet                                           │
│         │                                               │
│         │    ┌──────────────┐    ┌──────────────┐       │
│         └──> │   HTTPS:443  │───>│ Team Members │       │
│              │   (with TLS) │    │   Devices    │       │
│              └──────────────┘    └──────────────┘       │
└─────────────────────────────────────────────────────────┘
```

ts-plug:
1. Starts your upstream server process
2. Connects to your tailnet
3. Provisions TLS certificates automatically
4. Listens for connections on your tailnet
5. Reverse proxies traffic to your local server

## Configuration Flags

### Required

- Command after `--` - The server command to execute

### Network

- `-hostname` / `-hn` - Hostname on your tailnet (default: "tsmultiplug")
  ```sh
  ts-plug -hostname myapp -- python app.py
  # Access at: https://myapp.tailnet-name.ts.net
  ```

- `-dir` - Directory to store Tailscale state (default: ".data")
  ```sh
  ts-plug -dir /var/lib/tsplug -hostname api -- ./server
  ```

### Listeners

By default, ts-plug enables HTTPS on port 443 proxying to localhost:8080.

#### HTTP

- `-http` - Enable HTTP listener (default port mapping: 80:8080)
  ```sh
  # Enable HTTP, proxy port 80 to localhost:8080
  ts-plug -http -hostname web -- python -m http.server 8080
  ```

- `-http-port` - Customize HTTP port mapping
  ```sh
  # Listen on port 8000, proxy to localhost:3000
  ts-plug -http-port 8000:3000 -hostname web -- node server.js

  # Listen and proxy both on port 9000
  ts-plug -http-port 9000 -hostname web -- ./server
  ```

#### HTTPS

- `-https` - Enable HTTPS listener (default port mapping: 443:8080)
  ```sh
  ts-plug -https -hostname secure -- python -m http.server 8080
  ```

- `-https-port` - Customize HTTPS port mapping
  ```sh
  # Listen on port 8443, proxy to localhost:3000
  ts-plug -https-port 8443:3000 -hostname web -- node server.js
  ```

#### TCP

Raw TCP forwarding for protocols that aren't HTTP — SSH, databases, custom binary protocols, etc. No TLS termination, no header injection, just bytes. Access is gated by tailnet membership.

- `-tcp` - Enable TCP listener (default port mapping: 22:22)
  ```sh
  # Expose sshd on the tailnet (sshd already running locally on :22)
  ts-plug -tcp -hostname pi -- sleep infinity
  # Then from any tailnet device: ssh user@pi.tailnet-name.ts.net
  ```

- `-tcp-port` - Customize TCP port mapping
  ```sh
  # Expose Postgres on tailnet :5432 -> localhost:5432
  ts-plug -tcp-port 5432 -hostname db -- sleep infinity
  ```

- Unix socket upstreams — the `out` side of any tcp/http/https mapping can be `unix:<absolute path>` instead of a port:
  ```sh
  # Tailnet :22 -> sshd's AF_UNIX socket (systemd 256+, no TCP sshd needed)
  ts-plug -tcp-port 22:unix:/run/ssh-unix-local/socket -hostname xps13-ssh -- sleep infinity

  # Tailnet :2375 -> local docker daemon socket
  ts-plug -tcp-port 2375:unix:/var/run/docker.sock -hostname dockerbox -- sleep infinity
  ```
  Not supported for `-dns-port` (UDP vs stream socket).

#### DNS

- `-dns` - Enable DNS listener (default port mapping: 53:53)
  ```sh
  ts-plug -dns -hostname dns -- pihole-FTL
  ```

- `-dns-port` - Customize DNS port mapping
  ```sh
  # Forward DNS from port 53 to localhost:5353
  ts-plug -dns-port 53:5353 -hostname resolver -- dnsmasq
  ```

### Public Access

- `-public` - Enable Tailscale Funnel for public HTTPS access
  ```sh
  ts-plug -public -hostname demo -- python -m http.server 8080
  # Now accessible from the public internet!
  ```

  This is perfect for:
  - Webhook testing
  - Demo sites
  - Temporary public APIs
  - Sharing work with clients

### Debugging

- `-log` - Set log level (debug, info, warn, error)
  ```sh
  ts-plug -log debug -hostname myapp -- node server.js
  ```

- `-debug-tsnet` - Enable verbose tsnet.Server logging
  ```sh
  ts-plug -debug-tsnet -hostname myapp -- ./server
  ```

## Advanced Usage

### Multiple Listeners

Enable multiple protocols simultaneously:
```sh
# HTTP, HTTPS, and DNS
ts-plug -http -https -dns -hostname multi -- ./server
```

### Custom Port Mappings

Map different ports for each protocol:
```sh
ts-plug \
  -http-port 80:3000 \
  -https-port 443:3000 \
  -hostname myapp \
  -- node server.js
```

### Environment Detection

Your server can detect when it's running under ts-plug:
```sh
if [ "$TSPLUG_ACTIVE" = "1" ]; then
  echo "Running behind ts-plug!"
fi
```

```python
import os
if os.getenv('TSPLUG_ACTIVE') == '1':
    print("Running behind ts-plug!")
```

## Security Considerations

### Automatic TLS

ts-plug automatically provisions valid TLS certificates for your tailnet hostname. No configuration needed.

### User Identity Headers

When requests come through ts-plug, these headers are added:
- `Tailscale-User-Login` - User's login email
- `Tailscale-User-Name` - User's display name
- `Tailscale-User-Profile-Pic` - URL to user's profile picture

Your server can use these for authentication:

```python
@app.route('/api/whoami')
def whoami():
    return {
        'login': request.headers.get('Tailscale-User-Login'),
        'name': request.headers.get('Tailscale-User-Name'),
        'picture': request.headers.get('Tailscale-User-Profile-Pic')
    }
```

### Network Isolation

Services are only accessible to devices on your tailnet (unless `-public` is used).

## Use Cases

### Local Development Sharing

Share your dev server with teammates:
```sh
ts-plug -hostname dev-alice -- npm run dev
# Tell your teammate to visit: https://dev-alice.tailnet.ts.net
```

### Webhook Testing

Test webhooks without ngrok:
```sh
ts-plug -public -hostname webhook-test -- python webhook_server.py
# Use the public URL in GitHub/Stripe/etc webhook settings
```

### Running as a systemd Service

[`scripts/install-systemd.sh`](../scripts/install-systemd.sh) installs any ts-plug instance as a systemd service in one line. It finds a binary (in order: `--binary`, local `build/`, build from source, already-installed, download from [GitHub releases](https://github.com/th3wingman/ts-plug/releases) with sha256 verification), writes a shared `ts-plug@.service` template unit, and prompts for the auth key if you don't pass one.

No clone needed — run it straight from the repo URL (piped stdin disables the key prompt, so pass `--authkey` or `TS_AUTHKEY`):

```sh
curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
  | sudo bash -s -- ts-plug --name pi4-ssh --port 22 --authkey tskey-auth-...
```

Downloads support `--version vX.Y.Z` to pin a release and `--arch amd64|arm64|armv7` to override detection (e.g. 64-bit kernel with 32-bit userland on a Pi). From a clone:

```sh
# Expose local sshd at my-laptop-ssh.<tailnet>.ts.net:22
sudo scripts/install-systemd.sh ts-plug --name my-laptop-ssh --port 22

# Expose local Grafana at https://grafana.<tailnet>.ts.net
sudo scripts/install-systemd.sh ts-plug --name grafana --proto https --dst-port 3000

# Supervise the upstream too, instead of forwarding to something already running
sudo scripts/install-systemd.sh ts-plug --name hello --proto https --dst-port 8080 \
  --run '/usr/bin/python3 -m http.server 8080'
```

Flags: `--proto tcp|http|https|dns` (default `tcp`, default port 22), `--port N` (both sides), `--src-port`/`--dst-port` for asymmetric mappings, `--public` for Funnel (https only), `--args '...'` as a raw escape hatch. Each instance gets:

- `ts-plug@<name>` systemd unit (template shared by all instances)
- `/etc/ts-plug/<name>.env` — `TS_AUTHKEY` + generated args (mode 0600)
- `/var/lib/ts-plug/<name>/` — tsnet state: node keys, certs (`DynamicUser` + `StateDirectory`)

Multiple instances coexist (`ts-plug@ssh`, `ts-plug@grafana`, ...), each with its own tailnet identity. Once a node has joined, `TS_AUTHKEY` can be removed from the env file — identity persists in the state dir. Remove with `--uninstall` (keeps node keys) or `--uninstall --purge`.

The hostname defaults to `--name`; systemd specifiers like `%H` do **not** expand inside env files, so pass `--hostname` explicitly if it should differ.

#### Tailnet-only SSH, zero open TCP ports

With systemd ≥ 256 and openssh-server installed, `systemd-ssh-generator` provides sshd on a unix socket (`sshd-unix-local.socket` → `/run/ssh-unix-local/socket`). Point ts-plug at that socket and disable TCP sshd entirely — ssh is then reachable *only* through your tailnet:

```sh
sudo apt install openssh-server
sudo systemctl disable --now ssh.service ssh.socket    # kill TCP sshd
systemctl status sshd-unix-local.socket                # the unix one stays

sudo scripts/install-systemd.sh ts-plug --name xps13-ssh \
  --src-port 22 --dst-socket /run/ssh-unix-local/socket
# from any tailnet device: ssh user@xps13-ssh.<tailnet>.ts.net
```

Nothing listens on LAN :22; the only way in is tailnet membership. If the socket on your distro isn't world-connectable, add `--group` with its owning group.

### Headless Deployment (Raspberry Pi, etc.)

`make deploy` cross-compiles for arm64, ships the binary plus the install script over SSH, and runs the installer on the target (instance name = remote hostname, default `--port 22`):

```sh
make deploy HOST=192.168.0.21 TS_AUTHKEY=tskey-auth-xxxx
make deploy HOST=pi.local ENV_FILE=./secrets/tsplug.env PLUG_FLAGS='--proto https --dst-port 3000'
make deploy HOST=192.168.0.21          # node already joined; reuses state
```

### Container Deployment

Use as an entrypoint to eliminate sidecar containers:
```dockerfile
COPY ts-plug /usr/local/bin/
ENTRYPOINT ["ts-plug", "-hostname", "myapp", "--"]
CMD ["python", "app.py"]
```

See [docker.md](./docker.md) for detailed examples.

### Multi-Protocol Services

Run Pi-hole with both DNS and HTTP:
```sh
ts-plug \
  -dns \
  -http \
  -hostname pihole \
  -- pihole-FTL
```

## Troubleshooting

### Port Already in Use

If you get "address already in use", another process is listening on the configured port:
```sh
# Check what's using port 8080
lsof -i :8080

# Use a different port
ts-plug -https-port 443:3000 -hostname myapp -- node server.js
```

### Connection Refused

If ts-plug can't connect to your server:
1. Verify your server is listening on the correct port
2. Make sure it's listening on `0.0.0.0` or `127.0.0.1`, not a specific IP
3. Check logs with `-log debug`

### Tailscale Authentication

First run will prompt you to authenticate with Tailscale:
```sh
ts-plug -hostname test -- python -m http.server 8080
# Follow the URL to authenticate
```

State is saved in the `-dir` location (default: `.data/`)

## Examples

### Next.js Development
```sh
ts-plug -hostname nextjs-dev -https-port 443:3000 -- npm run dev
```

### Django Application
```sh
ts-plug -hostname django -https-port 443:8000 -- python manage.py runserver
```

### Static Site
```sh
ts-plug -public -hostname my-site -- python -m http.server 8080
```

### API with Custom Domain
```sh
ts-plug -hostname api-v1 -https-port 443:5000 -- flask run
```

## Comparison with ts-unplug

| Feature | ts-plug | ts-unplug |
|---------|---------|-----------|
| Direction | Local → tailnet | tailnet → Local |
| Use Case | Share local services | Access remote services |
| Starts Process | Yes | No |
| TLS | Automatic | Proxies existing |
| Public Access | Optional | No |

## See Also

- [ts-unplug Guide](./ts-unplug.md) - Access remote services locally
- [Use Cases](./use-cases.md) - Real-world patterns
- [Docker Examples](./docker.md) - Container integration
- [Main README](../README.md) - Quick start guide
