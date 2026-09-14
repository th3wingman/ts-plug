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
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// Tailnet ties one stock tsnet node to its own TUN + forwarder, and carries the
// handles the control plane needs to answer queries.
//
// Nodes are persistent (state survives restarts) and authenticate with a
// one-time browser login — the Cauldron connector model — so no authkeys are
// involved and identities never churn.
type Tailnet struct {
	conf       TailnetConf
	ts         *tsnet.Server
	tun        *os.File
	dev        string
	lc         *local.Client
	suffix     string
	stateDir   string // resolved tsnet state dir; selection pins live next to it
	assignedIP string
	resolve    resolveFunc
	resolved   *resolvedSync // systemd-resolved registration; nil in tests

	mu       sync.Mutex
	state    string // ipnstate backend state: NeedsLogin, Running, …
	loginURL string
	applied  int // selected resources currently in the hosts block
}

// ambientAuthEnvs are process-wide credentials tsnet would silently use for
// ANY tailnet. Reject them: with multiple tailnets, an ambient key would
// enroll this node into the wrong tailnet (Cauldron's cross-enrollment guard).
var ambientAuthEnvs = []string{"TS_AUTHKEY", "TS_AUTH_KEY", "TS_CLIENT_SECRET", "TS_CLIENT_ID", "TS_ID_TOKEN", "TS_AUDIENCE"}

func startTailnet(ctx context.Context, conf TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
	for _, key := range ambientAuthEnvs {
		if os.Getenv(key) != "" {
			return nil, fmt.Errorf("tailnet %q: %s is set — nodes use persistent state + browser login (`ts-multinet login %s`), unset it to avoid enrolling into the wrong tailnet", conf.Name, key, conf.Name)
		}
	}

	dir := conf.StateDir
	if dir == "" {
		dir = filepath.Join(baseDir, conf.Name)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}

	ts := &tsnet.Server{
		Hostname: "ts-multinet-" + conf.Name,
		Dir:      dir,
	}
	// Start (not Up): Up blocks until logged in, but tailnets boot into
	// NeedsLogin on first run — the daemon stays up and the watcher reports
	// the login URL. State in Dir makes later starts go straight to Running.
	if err := ts.Start(); err != nil {
		return nil, fmt.Errorf("tsnet start: %w", err)
	}
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

	// A pinned suffix is known from the start; otherwise it is auto-detected
	// from the netmap once the tailnet is Running. The friendly domain follows
	// the config (registerDomain keeps DNS in sync when it changes).
	suffix := strings.TrimSuffix(strings.ToLower(conf.Suffix), ".")
	if suffix != "" {
		reg.registerSuffix(conf.Name, suffix)
	}
	reg.registerDomain(conf.Name, conf.domainName())

	resolve := newTailnetResolver(lc)
	reg.registerResolver(conf.Name, resolve)
	tn := &Tailnet{conf: conf, ts: ts, tun: tun, dev: dev, lc: lc, suffix: suffix, stateDir: dir, resolve: resolve, resolved: rs}

	fwd := newForwarder(conf.Name, tun, mtu, reg, ts.Dial, resolve, newPinger(lc))
	go func() {
		if err := fwd.run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("forwarder exited", "name", conf.Name, "err", err)
		}
	}()

	go tn.watch(ctx, reg, onRunning)
	slog.Info("tailnet up", "name", conf.Name, "tun", dev, "cidr", conf.CIDR, "suffix", orDefault(suffix, "(auto)"))
	return tn, nil
}

// watch polls the tailnet's backend state: it records login state for the
// control plane, registers the MagicDNS suffix and the systemd-resolved link
// config once known, places the assigned tailnet IP on the TUN once Running,
// and fires onRunning (selections → hosts rewrite) on the transition to
// Running. resolved registration re-runs (cheap, idempotent) so a domain
// change through the control plane lands on the next poll.
func (t *Tailnet) watch(ctx context.Context, reg *registry, onRunning func()) {
	var suffixDone, addrDone, runningNotified bool
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		st, err := t.lc.Status(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("status poll failed", "name", t.conf.Name, "err", err)
		} else {
			t.mu.Lock()
			t.state = st.BackendState
			t.loginURL = st.AuthURL
			t.mu.Unlock()

			if t.suffix == "" && st.MagicDNSSuffix != "" {
				t.suffix = strings.TrimSuffix(strings.ToLower(st.MagicDNSSuffix), ".")
				reg.registerSuffix(t.conf.Name, t.suffix)
				suffixDone = true
				slog.Info("detected MagicDNS suffix", "name", t.conf.Name, "suffix", t.suffix)
			}
			if st.BackendState == "Running" {
				if !suffixDone && t.suffix != "" {
					suffixDone = true
				}
				if !addrDone {
					if ip4, _ := t.ts.TailscaleIPs(); ip4.IsValid() {
						t.mu.Lock()
						t.assignedIP = ip4.String()
						t.mu.Unlock()
						if err := addAddr(t.dev, ip4.String()+"/32"); err != nil {
							slog.Warn("could not add assigned IP to TUN", "name", t.conf.Name, "ip", ip4, "err", err)
						} else {
							slog.Info("assigned IP on TUN", "name", t.conf.Name, "ip", ip4.String(), "dev", t.dev)
						}
						addrDone = true
					}
				}
				if suffixDone && addrDone {
					t.resolved.register(t.dev, t.suffix, t.conf.domainName())
				}
				if !runningNotified && addrDone {
					runningNotified = true
					if onRunning != nil {
						onRunning()
					}
				}
			} else if st.BackendState == "NeedsLogin" && st.AuthURL != "" {
				slog.Info("tailnet needs login", "name", t.conf.Name, "login_url", st.AuthURL)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// status returns a snapshot of the tailnet's backend state and login URL.
func (t *Tailnet) status() (state, loginURL string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state, t.loginURL
}

// setApplied records how many of this tailnet's selections are live.
func (t *Tailnet) setApplied(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.applied = n
}

func (t *Tailnet) selectedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.applied
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
