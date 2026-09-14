// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// resolved.go — hand per-tailnet name resolution to systemd-resolved on the
// host: each TUN gets our DNS responder as its link DNS with routing domains
// for the MagicDNS suffix and the friendly domain (the same pattern tailscaled
// uses), so everything else on the host keeps its normal resolvers. Disabled
// inside containers (resolv.conf is ours there) and whenever the DNS listener
// isn't up — then the /etc/hosts block is the only resolution path.
package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// resolvedSync tracks which TUNs we configured so shutdown can revert them.
// A daemon killed without a chance to revert leaves stale per-link config on
// an interface that vanishes with the process; the next start re-registers.
type resolvedSync struct {
	mu         sync.Mutex
	addr       string            // host[:port] of our responder, as resolvectl wants it
	enabled    bool              // host mode + resolved present + DNS listener up
	registered map[string]string // dev -> "suffix domain" last applied
}

// resolvedPresent reports whether systemd-resolved is running on this host.
func resolvedPresent() bool {
	_, err := os.Stat("/run/systemd/resolve")
	return err == nil
}

// resolvectlAddr renders a listen address for resolvectl: the bare IP for the
// default port (older systemd rejects a :53 suffix), host:port otherwise.
func resolvectlAddr(addr string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil && port == "53" {
		return host
	}
	return addr
}

func newResolvedSync(addr string, dnsUp bool) *resolvedSync {
	return &resolvedSync{
		addr:       resolvectlAddr(addr),
		enabled:    dnsUp && !inContainer() && resolvedPresent(),
		registered: make(map[string]string),
	}
}

// register points dev's link DNS at our responder for suffix and domain.
// Idempotent; re-registers only when either changed (config edits land via
// the tailnet watcher re-reading tn.conf). Failures warn and move on — the
// hosts block still resolves selected names.
func (s *resolvedSync) register(dev, suffix, domain string) {
	if s == nil || !s.enabled || dev == "" || suffix == "" {
		return
	}
	key := suffix + " " + domain
	s.mu.Lock()
	if s.registered[dev] == key {
		s.mu.Unlock()
		return
	}
	s.registered[dev] = key
	s.mu.Unlock()

	if err := resolvectl("dns", dev, s.addr); err != nil {
		slog.Warn("resolvectl dns failed — selected names still resolve via the hosts block", "dev", dev, "err", err)
		return
	}
	if err := resolvectl("domain", dev, "~"+suffix, "~"+domain); err != nil {
		slog.Warn("resolvectl domain failed — selected names still resolve via the hosts block", "dev", dev, "err", err)
		return
	}
	slog.Info("registered with systemd-resolved", "dev", dev, "suffix", suffix, "domain", domain)
}

// revertAll returns every registered interface to its pre-daemon DNS config.
func (s *resolvedSync) revertAll() {
	if s == nil || !s.enabled {
		return
	}
	s.mu.Lock()
	devs := make([]string, 0, len(s.registered))
	for dev := range s.registered {
		devs = append(devs, dev)
	}
	s.registered = make(map[string]string)
	s.mu.Unlock()
	for _, dev := range devs {
		if err := resolvectl("revert", dev); err != nil {
			slog.Warn("resolvectl revert failed", "dev", dev, "err", err)
		}
	}
}

// revert returns one interface to its pre-daemon DNS config — the per-TUN
// counterpart of revertAll, used when a single tailnet stops at runtime.
func (s *resolvedSync) revert(dev string) {
	if s == nil || !s.enabled || dev == "" {
		return
	}
	s.mu.Lock()
	if _, ok := s.registered[dev]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.registered, dev)
	s.mu.Unlock()
	if err := resolvectl("revert", dev); err != nil {
		slog.Warn("resolvectl revert failed", "dev", dev, "err", err)
	}
}

func resolvectl(args ...string) error {
	out, err := exec.Command("resolvectl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("resolvectl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
