# Build context is the repo root (needs go.mod/go.sum):
#   docker build -f cmd/ts-multinet/Dockerfile -t ts-multinet .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static (CGO off) so it runs on Alpine/musl unchanged.
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/ts-multinet ./cmd/ts-multinet

FROM alpine:3.21
# iproute2 is REQUIRED (the daemon shells out to `ip`); the rest is a human
# toolbox so `docker exec -it <c> bash` is actually useful for validation.
RUN apk add --no-cache \
    ca-certificates iproute2 \
    bash curl wget bind-tools iputils netcat-openbsd tcpdump jq less

COPY --from=build /out/ts-multinet /usr/local/bin/ts-multinet
COPY cmd/ts-multinet/config.example.json /etc/ts-multinet/config.json

# Daemon needs --cap-add NET_ADMIN --device /dev/net/tun at runtime.
# Subcommands append cleanly: `docker run ... ts-multinet peers`.
# For a shell: `docker exec -it <container> bash`.
ENTRYPOINT ["ts-multinet", "-config", "/etc/ts-multinet/config.json"]
