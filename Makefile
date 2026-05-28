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


all: examples binaries

binaries: ts-plug ts-unplug ts-router

ts-plug:
	go build -o build/ts-plug ./cmd/ts-multi-plug

ts-unplug:
	go build -o build/ts-unplug ./cmd/ts-unplug

ts-router:
	go build -o build/ts-router ./cmd/ts-router

# ts-multinet is Linux-only (raw TUN + gVisor) and experimental; not in `all`.
ts-multinet:
	go build -o build/ts-multinet ./cmd/ts-multinet

docker-ts-multinet:
	docker build -f cmd/ts-multinet/Dockerfile -t ts-multinet .

darwin: darwin-ts-plug darwin-ts-unplug darwin-ts-router

darwin-ts-plug:
	GOOS=darwin GOARCH=arm64 go build -o build/ts-plug-darwin-arm64 ./cmd/ts-multi-plug

darwin-ts-unplug:
	GOOS=darwin GOARCH=arm64 go build -o build/ts-unplug-darwin-arm64 ./cmd/ts-unplug

darwin-ts-router:
	GOOS=darwin GOARCH=arm64 go build -o build/ts-router-darwin-arm64 ./cmd/ts-router

linux: linux-ts-plug linux-ts-unplug linux-ts-router

linux-ts-plug:
	GOOS=linux GOARCH=arm64 go build -o build/ts-plug-linux-arm64 ./cmd/ts-multi-plug
	GOOS=linux GOARCH=amd64 go build -o build/ts-plug-linux-amd64 ./cmd/ts-multi-plug

linux-ts-unplug:
	GOOS=linux GOARCH=arm64 go build -o build/ts-unplug-linux-arm64 ./cmd/ts-unplug
	GOOS=linux GOARCH=amd64 go build -o build/ts-unplug-linux-amd64 ./cmd/ts-unplug

linux-ts-router:
	GOOS=linux GOARCH=arm64 go build -o build/ts-router-linux-arm64 ./cmd/ts-router
	GOOS=linux GOARCH=amd64 go build -o build/ts-router-linux-amd64 ./cmd/ts-router

# Raspberry Pi 4 (64-bit Raspberry Pi OS / Ubuntu) — arm64.
# Use `pi` for the full set, or `pi-ts-plug` for just the plug binary.
pi: pi-ts-plug pi-ts-unplug pi-ts-router

pi-ts-plug:
	GOOS=linux GOARCH=arm64 go build -o build/ts-plug-linux-arm64 ./cmd/ts-multi-plug

pi-ts-unplug:
	GOOS=linux GOARCH=arm64 go build -o build/ts-unplug-linux-arm64 ./cmd/ts-unplug

pi-ts-router:
	GOOS=linux GOARCH=arm64 go build -o build/ts-router-linux-arm64 ./cmd/ts-router

install: binaries
	cp build/ts-plug $(GOPATH)/bin/ts-plug
	cp build/ts-unplug $(GOPATH)/bin/ts-unplug
	cp build/ts-router $(GOPATH)/bin/ts-router

# Install ts-router system-wide and grant cap_net_bind_service so it can
# bind :80/:443 without running as root. Override PREFIX or BINDIR to
# change the install location.
install-ts-router: ts-router
	sudo install -m 0755 build/ts-router $(BINDIR)/ts-router
	sudo setcap 'cap_net_bind_service=+ep' $(BINDIR)/ts-router

clean:
	rm -rf $(BUILD_DIR)/*

examples:
	go build -o $(BUILD_DIR)/hello ./cmd/examples/hello/hello.go
	go build -o $(BUILD_DIR)/resolver ./cmd/examples/resolver/resolver.go

# use cached test results while developing
test: examples
#	go test -race -timeout 30s -short ./internal/...
	staticcheck ./... || true

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

.PHONY: all test examples clean binaries ts-plug ts-unplug ts-router ts-multinet docker-ts-multinet darwin darwin-ts-plug darwin-ts-unplug darwin-ts-router linux linux-ts-plug linux-ts-unplug linux-ts-router pi pi-ts-plug pi-ts-unplug pi-ts-router install install-ts-router
