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
	"tailscale.com/ipn"
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
	resolved   *resolvedSync   // systemd-resolved registration; nil in tests
	cancel     func()          // cancels this tailnet's ctx (watcher + forwarder)
	wg         *sync.WaitGroup // forwarder + watcher lifetime; Close waits before dropping the TUN

	mu       sync.Mutex
	state    string // ipnstate backend state: NeedsLogin, Running, …
	loginURL string
	applied  int  // selected resources currently in the hosts block
	fwdDead  bool // forwarder datapath exited unexpectedly (watchdog restarts)
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
		Hostname: conf.nodeHostname(),
		Dir:      dir,
		AuthKey:  conf.AuthKey, // tagged-device enrollment: tsnet reads it at start, no browser flow (ambient env keys stay rejected above)
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

	// Per-tailnet ctx so one tailnet can stop without touching the others:
	// the watcher and the forwarder both exit on its cancel. All the fallible
	// setup is above, so a failed start never leaks a goroutine pair.
	tctx, cancel := context.WithCancel(ctx)
	tn := &Tailnet{conf: conf, ts: ts, tun: tun, dev: dev, lc: lc, suffix: suffix, stateDir: dir, resolved: rs, cancel: cancel, wg: &sync.WaitGroup{}}

	// Peers and advertised services both resolve through this node; the suffix
	// accessor lets a service FQDN be matched exactly once the suffix is known.
	resolve := newTailnetResolver(lc, func() string { return tn.suffix })
	tn.resolve = resolve
	reg.registerResolver(conf.Name, resolve)

	fwd := newForwarder(conf.Name, tun, mtu, reg, ts.Dial, resolve, newPinger(lc))
	tn.wg.Add(3) // forwarder + watcher + netmap subscriber: Close waits for all before the TUN fd drops
	go func() {
		defer tn.wg.Done()
		if err := fwd.run(tctx); err != nil && tctx.Err() == nil {
			slog.Error("forwarder exited — marking the tailnet unhealthy for restart", "name", conf.Name, "err", err)
			tn.setForwarderDead()
		}
	}()

	go func() {
		defer tn.wg.Done()
		tn.watch(tctx, reg, onRunning)
	}()

	go func() {
		defer tn.wg.Done()
		tn.watchNetmap(tctx, onRunning)
	}()
	slog.Info("tailnet up", "name", conf.Name, "tun", dev, "cidr", conf.CIDR, "suffix", orDefault(suffix, "(auto)"))
	if w := tldWarning(conf.domainName()); w != "" {
		slog.Warn(w, "name", conf.Name)
	}
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

// watchNetmap re-applies selections when the control plane pushes a netmap
// change that can alter what the auto-select rules match: a peer added,
// replaced, or removed (tag changes always travel as full-node
// PeersChanged), or this node's own capabilities changed (advertised-service
// visibility lives in them). The bus is push — no polling — so a device
// tagged in the admin console lands on the next control update, and an
// NotifyPeerPatches keeps the cheap shape: online/offline flaps arrive as
// narrow PeerChangedPatch (ignored — they can't change a match) instead of
// being promoted to full-node PeersChanged, which would re-run applySelections
// on every flap. (ipn.NotifyRateLimit is deliberately absent: it is a
// legacy-netmap bit that localapi rejects in combination with the delta
// bits — a 400 on every (re)subscribe.) A dropped stream is resubscribed — a
// dead backend is the health watchdog's problem, not ours.
func (t *Tailnet) watchNetmap(ctx context.Context, onRunning func()) {
	for ctx.Err() == nil {
		w, err := t.lc.WatchIPNBus(ctx, ipn.NotifyPeerChanges|ipn.NotifyPeerPatches)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("netmap watch failed — retrying", "name", t.conf.Name, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for {
			n, err := w.Next()
			if err != nil {
				w.Close()
				if ctx.Err() != nil {
					return
				}
				slog.Warn("netmap watch ended — resubscribing", "name", t.conf.Name, "err", err)
				break
			}
			if autoNotifyApplies(n) && onRunning != nil {
				onRunning() // applySelections: idempotent, d.mu-serialized
			}
		}
	}
}

// autoNotifyApplies reports whether an IPN notification can change what the
// auto-select rules match: full peer add/replace/remove (tags ride whole
// nodes — patches never carry them), or a self change (this node's
// capabilities hold the advertised-service visibility). Pure, so the trigger
// is unit-testable without a bus.
func autoNotifyApplies(n ipn.Notify) bool {
	return n.SelfChange != nil || len(n.PeersChanged) > 0 || len(n.PeersRemoved) > 0
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

// setForwarderDead marks the datapath as gone so the daemon's health watch
// restarts this tailnet (a TUN error otherwise leaves it up but dark).
func (t *Tailnet) setForwarderDead() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fwdDead = true
}

// forwarderDead reports whether the datapath exited unexpectedly.
func (t *Tailnet) forwarderDead() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fwdDead
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
// own peer status and advertised-service list. This deliberately avoids the
// system resolver (which we've pointed at our own synthetic-IP responder).
func newTailnetResolver(lc *local.Client, suffix func() string) resolveFunc {
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
		// An advertised VIP service resolves to its own address; the forwarder
		// dials that through the tailnet like any peer target.
		svcs, err := lc.GetServices(ctx)
		if err != nil {
			return "", false
		}
		sfx := suffix()
		for name, d := range svcs {
			if serviceFQDN(name, sfx) == want && len(d.Addrs) > 0 {
				return d.Addrs[0].String(), true
			}
		}
		return "", false
	}
}

// Close stops the tailnet: cancel its goroutines, then the TUN (the kernel
// drops the link and its route) and the tsnet node. Safe to call twice and
// on test stubs (nil fields, no cancel).
func (t *Tailnet) Close() {
	if t.cancel != nil {
		t.cancel()
	}
	// Wait for the forwarder and watcher to exit before closing the TUN: the
	// interface lives until every fd reference drops, so closing early makes
	// an immediate stop/start of the same tun name fail with TUNSETIFF EBUSY.
	if t.wg != nil {
		t.wg.Wait()
	}
	if t.tun != nil {
		t.tun.Close()
	}
	if t.ts != nil {
		t.ts.Close()
	}
}
