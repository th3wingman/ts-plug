# Build context is the repo root (needs go.mod/go.sum):
#   docker build -f cmd/ts-multinet/Dockerfile -t ts-multinet .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/ts-multinet ./cmd/ts-multinet

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends iproute2 ca-certificates curl iputils-ping \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ts-multinet /usr/local/bin/ts-multinet
COPY cmd/ts-multinet/config.example.json /etc/ts-multinet/config.json

# Needs --cap-add NET_ADMIN --device /dev/net/tun at runtime.
ENTRYPOINT ["ts-multinet", "-config", "/etc/ts-multinet/config.json"]
