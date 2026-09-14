// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
)

// The web UI is a dependency-free single page (vanilla JS, no build step)
// served same-origin with the control API — the browser talks to the exact
// endpoints the CLI does.
//
//go:embed all:web
var webAssets embed.FS

// uiHandler serves the embedded web UI; index.html is the SPA entry point.
func uiHandler() http.Handler {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err) // the embedded FS is fixed at compile time; Sub cannot fail
	}
	return http.FileServer(http.FS(sub))
}

// uiMux is the control API plus the UI at "/": the API patterns are more
// specific and win. Only the TCP listener serves the UI — the unix control
// socket stays API-only.
func (d *Daemon) uiMux() *http.ServeMux {
	mux := d.controlMux()
	mux.Handle("/", uiHandler())
	return mux
}

// serveUI runs the web UI listener until ctx is cancelled. A bind failure is
// never fatal — the daemon keeps working over the control socket.
func serveUI(ctx context.Context, d *Daemon, addr string) {
	if addr == "" {
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Warn("web ui disabled — listen failed (daemon continues)", "addr", addr, "err", err)
		return
	}
	srv := &http.Server{Handler: d.uiMux()}
	go func() { <-ctx.Done(); srv.Close() }()
	slog.Info("web ui up", "url", "http://"+addr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		slog.Error("web ui serve", "err", err)
	}
}
