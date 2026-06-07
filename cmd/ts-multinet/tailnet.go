// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// Tailnet ties one stock tsnet node to its own TUN + forwarder, and carries the
// handles the control plane needs to answer queries.
type Tailnet struct {
	conf       TailnetConf
	ts         *tsnet.Server
	tun        *os.File
	lc         *local.Client
	suffix     string
	assignedIP string
	resolve    resolveFunc
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

	lc, err := ts.LocalClient()
	if err != nil {
		ts.Close()
		return nil, fmt.Errorf("local client: %w", err)
	}

	tun, dev, err := openTUN(conf.TUN)
	if err != nil {
		ts.Close()
		return nil, err
	}
	if err := bringUp(dev, mtu); err != nil {
		tun.Close()
		ts.Close()
		return nil, err
	}
	if err := addRoute(conf.CIDR, dev); err != nil {
		tun.Close()
		ts.Close()
		return nil, err
	}

	// Put our assigned tailnet IP on the TUN so the kernel sources
	// synthetic-range traffic from it rather than from eth0.
	var assigned string
	if ip4, _ := ts.TailscaleIPs(); ip4.IsValid() {
		assigned = ip4.String()
		if err := addAddr(dev, assigned+"/32"); err != nil {
			slog.Warn("could not add assigned IP to TUN", "name", conf.Name, "ip", assigned, "err", err)
		}
	}

	slog.Info("tailnet ready", "name", conf.Name, "tun", dev, "cidr", conf.CIDR,
		"suffix", conf.Suffix, "ip", assigned)

	resolve := newTailnetResolver(lc)
	reg.registerResolver(conf.Name, resolve) // lets DNS verify peer existence (search fallthrough)
	tn := &Tailnet{conf: conf, ts: ts, tun: tun, lc: lc, suffix: conf.Suffix, assignedIP: assigned, resolve: resolve}

	fwd := newForwarder(conf.Name, tun, mtu, reg, ts.Dial, resolve, newPinger(lc))
	go func() {
		if err := fwd.run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("forwarder exited", "name", conf.Name, "err", err)
		}
	}()

	return tn, nil
}

// newPinger probes real reachability to a tailnet IP over the tailnet. TSMP
// traverses to the peer at the IP layer (works even if the peer firewalls
// ICMP) and yields a real round-trip latency.
func newPinger(lc *local.Client) func(ctx context.Context, ip netip.Addr) (time.Duration, bool) {
	return func(ctx context.Context, ip netip.Addr) (time.Duration, bool) {
		res, err := lc.Ping(ctx, ip, tailcfg.PingTSMP)
		if err != nil || res == nil || res.Err != "" || res.LatencySeconds <= 0 {
			return 0, false
		}
		return time.Duration(res.LatencySeconds * float64(time.Second)), true
	}
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
