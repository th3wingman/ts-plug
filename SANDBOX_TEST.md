# SANDBOX_TEST

## Summary

This project is a collection of Go command-line tools for exposing services to and from a [Tailscale](https://tailscale.com) tailnet. It provides `ts-plug` (expose a localhost service to your tailnet, optionally as a Docker entrypoint to avoid sidecars), `ts-unplug` (bring a tailnet service to localhost via reverse proxy), `ts-router` (surface many tailnet hosts on localhost under their real URLs), and the experimental `ts-multinet` (connect to several tailnets transparently on a single host at once). Together they make sharing and accessing services across one or more tailnets simple, with built-in TLS, DNS, and HTTP/HTTPS support.

## Top-Level Files and Directories

- `cmd/` — Go source for the command-line binaries (and examples)
- `docker/` — Docker integration examples (Pi-hole, Open WebUI, Audiobookshelf)
- `docs/` — Guides and detailed documentation
- `Makefile` — Build, install, and deploy targets
- `README.md` — Project overview and quick start
- `go.mod` / `go.sum` — Go module definition and dependency checksums
- `LICENSE` — License file
- `.gitignore` — Git ignore rules
