# Standalone ts-plug image — ts-plug as its own container, not baked into an app.
# Used as a sidecar that shares another container's network namespace
# (compose: network_mode "service:<app>"), proxying localhost:<port> onto the tailnet.
#
# Build context is the repo root (needs go.mod/go.sum):
#   docker build -f docker/standalone/Dockerfile -t ts-plug .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static (CGO off); tsnet is pure Go, no system deps.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ts-plug ./cmd/ts-multi-plug

# Debian (not Alpine) on purpose: the wrapped keepalive is `-- /bin/sleep infinity`,
# and only GNU coreutils sleep accepts "infinity" — busybox sleep wants a number.
FROM debian:trixie-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ts-plug /usr/local/bin/ts-plug
# tsnet state (node identity + certs) — mount a volume here to persist across restarts.
RUN mkdir -p /state
# Flags + the wrapped command come from the compose `command:`, e.g.
#   -hostname k3v1n -border0 -https-port 443:8181 -dir /state -- /bin/sleep infinity
ENTRYPOINT ["ts-plug"]
