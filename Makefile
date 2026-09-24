BUILD_DIR = build
PREFIX   ?= /usr/local
BINDIR   ?= $(PREFIX)/bin

# Get the current Git hash
GIT_HASH := $(shell git rev-parse --short HEAD)
ifneq ($(shell git status --porcelain),)
    # There are untracked changes
    GIT_HASH := $(GIT_HASH)+
endif

# Capture the current build date in RFC3339 format
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

.DEFAULT_GOAL := help

all: examples binaries ## Build everything portable: examples + the four binaries

binaries: ts-plug ts-unplug ts-router ts-unplug-proxy ## The four portable binaries (ts-multinet is separate — Linux, experimental)

ts-plug: ## Build ts-plug — expose localhost to your tailnet
	go build -o build/ts-plug ./cmd/ts-multi-plug

ts-unplug: ## Build ts-unplug — bring a tailnet service to localhost
	go build -o build/ts-unplug ./cmd/ts-unplug

ts-router: ## Build ts-router — many tailnet hosts on localhost, real URLs
	go build -o build/ts-router ./cmd/ts-router

ts-unplug-proxy: ## Build ts-unplug-proxy — SOCKS5/HTTP proxy into the tailnet
	go build -o build/ts-unplug-proxy ./cmd/ts-unplug-proxy

# ts-multinet is Linux-only (raw TUN + gVisor) and experimental; not in `all`.
ts-multinet: ## Build ts-multinet — several tailnets transparently at once (Linux, experimental)
	go build -o build/ts-multinet ./cmd/ts-multinet

docker-ts-multinet: ## Build the alpine ts-multinet runtime image
	docker build -f cmd/ts-multinet/Dockerfile -t ts-multinet .

darwin: darwin-ts-plug darwin-ts-unplug darwin-ts-router darwin-ts-unplug-proxy ## Cross-build all four for macOS arm64

darwin-ts-plug: ## ts-plug for macOS arm64
	GOOS=darwin GOARCH=arm64 go build -o build/ts-plug-darwin-arm64 ./cmd/ts-multi-plug

darwin-ts-unplug: ## ts-unplug for macOS arm64
	GOOS=darwin GOARCH=arm64 go build -o build/ts-unplug-darwin-arm64 ./cmd/ts-unplug

darwin-ts-router: ## ts-router for macOS arm64
	GOOS=darwin GOARCH=arm64 go build -o build/ts-router-darwin-arm64 ./cmd/ts-router

darwin-ts-unplug-proxy: ## ts-unplug-proxy for macOS arm64
	GOOS=darwin GOARCH=arm64 go build -o build/ts-unplug-proxy-darwin-arm64 ./cmd/ts-unplug-proxy

linux: linux-ts-plug linux-ts-unplug linux-ts-router linux-ts-unplug-proxy ## Cross-build all four for linux arm64+amd64

linux-ts-plug: ## ts-plug for linux arm64+amd64
	GOOS=linux GOARCH=arm64 go build -o build/ts-plug-linux-arm64 ./cmd/ts-multi-plug
	GOOS=linux GOARCH=amd64 go build -o build/ts-plug-linux-amd64 ./cmd/ts-multi-plug

linux-ts-unplug: ## ts-unplug for linux arm64+amd64
	GOOS=linux GOARCH=arm64 go build -o build/ts-unplug-linux-arm64 ./cmd/ts-unplug
	GOOS=linux GOARCH=amd64 go build -o build/ts-unplug-linux-amd64 ./cmd/ts-unplug

linux-ts-router: ## ts-router for linux arm64+amd64
	GOOS=linux GOARCH=arm64 go build -o build/ts-router-linux-arm64 ./cmd/ts-router
	GOOS=linux GOARCH=amd64 go build -o build/ts-router-linux-amd64 ./cmd/ts-router

linux-ts-unplug-proxy: ## ts-unplug-proxy for linux arm64+amd64
	GOOS=linux GOARCH=arm64 go build -o build/ts-unplug-proxy-linux-arm64 ./cmd/ts-unplug-proxy
	GOOS=linux GOARCH=amd64 go build -o build/ts-unplug-proxy-linux-amd64 ./cmd/ts-unplug-proxy

# Raspberry Pi 4 (64-bit Raspberry Pi OS / Ubuntu) — arm64.
# Use `pi` for the full set, or `pi-ts-plug` for just the plug binary.
pi: pi-ts-plug pi-ts-unplug pi-ts-router ## Cross-build the Pi set (arm64): plug, unplug, router

pi-ts-plug: ## ts-plug for the Pi (arm64)
	GOOS=linux GOARCH=arm64 go build -o build/ts-plug-linux-arm64 ./cmd/ts-multi-plug

pi-ts-unplug: ## ts-unplug for the Pi (arm64)
	GOOS=linux GOARCH=arm64 go build -o build/ts-unplug-linux-arm64 ./cmd/ts-unplug

pi-ts-router: ## ts-router for the Pi (arm64)
	GOOS=linux GOARCH=arm64 go build -o build/ts-router-linux-arm64 ./cmd/ts-router

# Install a tool as a systemd service on THIS machine via
# scripts/install-systemd.sh (builds the binary first if needed).
#
#   make install-service NAME=my-laptop-ssh FLAGS='--port 22'
#   make install-service TOOL=ts-unplug NAME=db FLAGS='--port 5432 --mode tcp db.tailnet.ts.net:5432'
#
# See `scripts/install-systemd.sh --help` for all flags.
TOOL ?= ts-plug
install-service: ## Install a tool as a systemd service on THIS machine (NAME=, TOOL=, FLAGS='…')
	@test -n "$(NAME)" || { echo "usage: make install-service NAME=<instance> [TOOL=ts-plug] [FLAGS='--port 22']"; exit 1; }
	sudo scripts/install-systemd.sh $(TOOL) --name $(NAME) $(FLAGS)

# Deploy ts-plug to a remote arm64 host (Raspberry Pi etc.) over SSH:
# build the arm64 binary, ship it plus scripts/install-systemd.sh, and run
# the installer there. Instance name = the remote hostname; default service
# is raw TCP forwarding of :22 (override with PLUG_FLAGS).
#
# The TS_AUTHKEY is handled one of three ways:
#   TS_AUTHKEY=tskey-...  passed to the installer via the remote environment
#   ENV_FILE=path         your local env file is copied and read on the host
#   (neither)             assumes the node already has state on the host
# ENV_FILE is the safer option — TS_AUTHKEY on the command line is visible
# in `ps` and shell history.
#
#   make deploy HOST=192.168.0.21 TS_AUTHKEY=tskey-auth-xxxx
#   make deploy HOST=pi.local ENV_FILE=./secrets/tsplug.env PLUG_FLAGS='--port 22'
#   make deploy HOST=192.168.0.21          # node already joined
SSH_USER ?= root
PLUG_FLAGS ?= --port 22
deploy: pi-ts-plug ## Ship ts-plug to a remote arm64 host over SSH (HOST=, ENV_FILE= or TS_AUTHKEY=)
	@test -n "$(HOST)" || { echo "usage: make deploy HOST=<ip-or-host> [SSH_USER=root] [TS_AUTHKEY=tskey-... | ENV_FILE=path] [PLUG_FLAGS='--port 22']"; exit 1; }
	scp build/ts-plug-linux-arm64 $(SSH_USER)@$(HOST):/tmp/ts-plug.bin
	scp scripts/install-systemd.sh $(SSH_USER)@$(HOST):/tmp/ts-plug-install.sh
	@if [ -n "$(ENV_FILE)" ]; then \
	  echo "copying env file $(ENV_FILE)"; \
	  scp "$(ENV_FILE)" $(SSH_USER)@$(HOST):/tmp/tsplug.env; \
	fi
	ssh $(SSH_USER)@$(HOST) 'set -e; chmod +x /tmp/ts-plug-install.sh; \
	  systemctl disable --now ts-plug.service 2>/dev/null && echo "disabled legacy ts-plug.service" || true; \
	  TS_AUTHKEY="$(TS_AUTHKEY)" /tmp/ts-plug-install.sh ts-plug \
	    --name "$$(hostname -s)" --binary /tmp/ts-plug.bin \
	    $(if $(ENV_FILE),--env-file /tmp/tsplug.env) $(PLUG_FLAGS) < /dev/null; \
	  rm -f /tmp/ts-plug.bin /tmp/tsplug.env /tmp/ts-plug-install.sh'

install: binaries ## Copy the four binaries into $GOPATH/bin
	cp build/ts-plug $(GOPATH)/bin/ts-plug
	cp build/ts-unplug $(GOPATH)/bin/ts-unplug
	cp build/ts-router $(GOPATH)/bin/ts-router
	cp build/ts-unplug-proxy $(GOPATH)/bin/ts-unplug-proxy

# Install ts-router system-wide and grant cap_net_bind_service so it can
# bind :80/:443 without running as root. Override PREFIX or BINDIR to
# change the install location.
install-ts-router: ts-router ## Install ts-router system-wide + cap_net_bind_service (PREFIX=, BINDIR=)
	sudo install -m 0755 build/ts-router $(BINDIR)/ts-router
	sudo setcap 'cap_net_bind_service=+ep' $(BINDIR)/ts-router

# Install (or upgrade) ts-multinet as a systemd service on THIS machine via
# scripts/install-ts-multinet.sh. The binary is built as the invoking user
# first, so the script never runs `go build` under root's cold cache; existing
# config and node state are kept, and the daemon is restarted.
#
#   make install-ts-multinet
#   make install-ts-multinet ARGS=--uninstall      # add ARGS=--purge to drop state
#   make install-ts-multinet ARGS="--config my.jsonc"
install-ts-multinet: ts-multinet ## Install/upgrade ts-multinet as a systemd service (config + state kept)
	sudo scripts/install-ts-multinet.sh --binary $(CURDIR)/build/ts-multinet $(ARGS)

clean: ## Remove build/ artifacts
	rm -rf $(BUILD_DIR)/*

examples: ## Build the example binaries (hello, resolver)
	go build -o $(BUILD_DIR)/hello ./cmd/examples/hello/hello.go
	go build -o $(BUILD_DIR)/resolver ./cmd/examples/resolver/resolver.go

# use cached test results while developing
test: examples ## Lint everything (staticcheck; unit tests live in cmd/ts-multinet)
#	go test -race -timeout 30s -short ./internal/...
	staticcheck ./... || true

help: ## Show this help (the default goal — `make <TAB>` completes targets via bash-completion)
	@grep -E '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-26s\033[0m %s\n", $$1, $$2}'

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

.PHONY: all test examples clean help binaries ts-plug ts-unplug ts-router ts-unplug-proxy ts-multinet docker-ts-multinet darwin darwin-ts-plug darwin-ts-unplug darwin-ts-router darwin-ts-unplug-proxy linux linux-ts-plug linux-ts-unplug linux-ts-router linux-ts-unplug-proxy pi pi-ts-plug pi-ts-unplug pi-ts-router deploy install install-ts-router install-ts-multinet install-service
