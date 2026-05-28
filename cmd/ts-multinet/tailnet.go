// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Tailnet ties one stock tsnet node to its own TUN + forwarder.
type Tailnet struct {
	conf TailnetConf
	ts   *tsnet.Server
	tun  *os.File
}

func startTailnet(ctx context.Context, conf TailnetConf, reg *registry, mtu uint32, baseDir string) (*Tailnet, error) {
	authkey := os.Getenv(conf.AuthKeyEnv)
	if authkey == "" {
		return nil, fmt.Errorf("env %s is empty", conf.AuthKeyEnv)
	}

	dir := conf.StateDir
	if dir == "" {
		dir = filepath.Join(baseDir, conf.Name)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}

	ts := &tsnet.Server{
		Hostname:  "ts-multinet-" + conf.Name,
		Dir:       dir,
		AuthKey:   authkey,
		Ephemeral: true,
	}
	st, err := ts.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("tsnet up: %w", err)
	}

	// Auto-detect the MagicDNS suffix unless the config pinned one.
	if conf.Suffix == "" {
		suffix := st.MagicDNSSuffix
		if st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSSuffix != "" {
			suffix = st.CurrentTailnet.MagicDNSSuffix
		}
		if suffix == "" {
			ts.Close()
			return nil, fmt.Errorf("could not determine MagicDNS suffix; set %q.suffix in config", conf.Name)
		}
		conf.Suffix = suffix
	}
	reg.registerSuffix(conf.Name, conf.Suffix)

	tun, dev, err := openTUN(conf.TUN)
	if err != nil {
		return nil, err
	}
	if err := bringUp(dev, mtu); err != nil {
		tun.Close()
		return nil, err
	}
	if err := addRoute(conf.CIDR, dev); err != nil {
		tun.Close()
		return nil, err
	}
	slog.Info("tailnet ready", "name", conf.Name, "tun", dev, "cidr", conf.CIDR, "suffix", conf.Suffix)

	lc, err := ts.LocalClient()
	if err != nil {
		tun.Close()
		return nil, fmt.Errorf("local client: %w", err)
	}
	fwd := newForwarder(conf.Name, tun, mtu, reg, ts.Dial, newTailnetResolver(lc))
	go func() {
		if err := fwd.run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("forwarder exited", "name", conf.Name, "err", err)
		}
	}()

	return &Tailnet{conf: conf, ts: ts, tun: tun}, nil
}

// newTailnetResolver resolves an FQDN to a real tailnet IP using the tailnet's
// own peer list. This deliberately avoids the system resolver (which we've
// pointed at our own synthetic-IP responder).
func newTailnetResolver(lc *local.Client) resolveFunc {
	match := func(p *ipnstate.PeerStatus, want string) (string, bool) {
		if p == nil || len(p.TailscaleIPs) == 0 {
			return "", false
		}
		if strings.TrimSuffix(strings.ToLower(p.DNSName), ".") == want {
			return p.TailscaleIPs[0].String(), true
		}
		return "", false
	}
	return func(ctx context.Context, host string) (string, bool) {
		want := strings.TrimSuffix(strings.ToLower(host), ".")
		st, err := lc.Status(ctx)
		if err != nil {
			return "", false
		}
		if ip, ok := match(st.Self, want); ok {
			return ip, true
		}
		for _, p := range st.Peer {
			if ip, ok := match(p, want); ok {
				return ip, true
			}
		}
		return "", false
	}
}

func (t *Tailnet) Close() {
	if t.tun != nil {
		t.tun.Close()
	}
	if t.ts != nil {
		t.ts.Close()
	}
}
