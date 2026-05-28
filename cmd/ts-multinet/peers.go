// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// probeTimeout bounds each per-port connection attempt.
const probeTimeout = 1500 * time.Millisecond

// bringUpForQuery starts a tailnet just far enough to query it: tsnet up, no
// TUN, no forwarder. Caller must Close the returned server. Runs in a throwaway
// container so it won't fight the main daemon for state locks.
func bringUpForQuery(ctx context.Context, tc TailnetConf, base string) (*tsnet.Server, *local.Client, *ipnstate.Status, error) {
	authkey := os.Getenv(tc.AuthKeyEnv)
	if authkey == "" {
		return nil, nil, nil, fmt.Errorf("%s is empty", tc.AuthKeyEnv)
	}
	dir := tc.StateDir
	if dir == "" {
		dir = filepath.Join(base, tc.Name)
	}
	ts := &tsnet.Server{Hostname: "ts-multinet-" + tc.Name, Dir: dir, AuthKey: authkey, Ephemeral: true}
	if _, err := ts.Up(ctx); err != nil {
		ts.Close()
		return nil, nil, nil, err
	}
	lc, err := ts.LocalClient()
	if err != nil {
		ts.Close()
		return nil, nil, nil, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		ts.Close()
		return nil, nil, nil, err
	}
	return ts, lc, st, nil
}

func suffixOf(st *ipnstate.Status) string {
	if st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSSuffix != "" {
		return st.CurrentTailnet.MagicDNSSuffix
	}
	return st.MagicDNSSuffix
}

// runPeers lists each tailnet's peers and, for online peers, probes a set of
// ports so you can see what's actually reachable. An optional name filter keeps
// the output sane on big tailnets.
func runPeers(ctx context.Context, cfg *Config, filter string, ports []int, probe bool) {
	base := orDefault(cfg.StateDir, ".state")
	for _, tc := range cfg.Tailnets {
		ts, _, st, err := bringUpForQuery(ctx, tc, base)
		if err != nil {
			fmt.Printf("\n== %s ==  (error: %v)\n", tc.Name, err)
			continue
		}
		suffix := suffixOf(st)

		peers := make([]*ipnstate.PeerStatus, 0, len(st.Peer))
		up := 0
		for _, p := range st.Peer {
			short := strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(p.DNSName), "."), "."+suffix)
			if filter != "" && !strings.Contains(short, strings.ToLower(filter)) {
				continue
			}
			peers = append(peers, p)
			if p.Online {
				up++
			}
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].DNSName < peers[j].DNSName })

		hint := ""
		if filter == "" && len(peers) > 25 {
			hint = "  (tip: pass a name filter, e.g. `peers connector`)"
		}
		fmt.Printf("\n== %s (%s) — %d shown, %d up ==%s\n", tc.Name, suffix, len(peers), up, hint)

		// Probe online peers concurrently.
		services := map[string][]int{}
		if probe && len(ports) > 0 {
			services = probePeers(ctx, ts.Dial, peers, ports)
		}

		fmt.Printf("  %-5s %-34s %-16s %-7s %s\n", "STATE", "NAME", "IP", "OS", "SERVICES")
		for _, p := range peers {
			short := strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(p.DNSName), "."), "."+suffix)
			state := "down"
			if p.Online {
				state = "UP"
			}
			ip := ""
			if len(p.TailscaleIPs) > 0 {
				ip = p.TailscaleIPs[0].String()
			}
			svc := "—"
			if open := services[ip]; len(open) > 0 {
				parts := make([]string, len(open))
				for i, port := range open {
					parts[i] = ":" + strconv.Itoa(port)
				}
				svc = strings.Join(parts, " ")
			}
			fmt.Printf("  %-5s %-34s %-16s %-7s %s\n", state, truncate(short, 34), ip, p.OS, svc)
		}
		ts.Close()
	}
}

// probePeers dials each online peer's ports over the tailnet, returning the open
// ports per IP. Concurrency-limited so big tailnets don't fan out unboundedly.
func probePeers(ctx context.Context, dial dialFunc, peers []*ipnstate.PeerStatus, ports []int) map[string][]int {
	type result struct {
		ip   string
		port int
	}
	sem := make(chan struct{}, 32)
	results := make(chan result, 256)
	var wg sync.WaitGroup

	for _, p := range peers {
		if !p.Online || len(p.TailscaleIPs) == 0 {
			continue
		}
		ip := p.TailscaleIPs[0].String()
		for _, port := range ports {
			wg.Add(1)
			go func(ip string, port int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				dctx, cancel := context.WithTimeout(ctx, probeTimeout)
				defer cancel()
				c, err := dial(dctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
				if err == nil {
					c.Close()
					results <- result{ip, port}
				}
			}(ip, port)
		}
	}
	go func() { wg.Wait(); close(results) }()

	open := map[string][]int{}
	for r := range results {
		open[r.ip] = append(open[r.ip], r.port)
	}
	for ip := range open {
		sort.Ints(open[ip])
	}
	return open
}

// runCheck walks the full transparent path for one target and reports each step
// in plain English: which tailnet, what it resolved to, and the dial outcome.
func runCheck(ctx context.Context, cfg *Config, target string, base string) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		host, portStr = target, "80"
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		fmt.Printf("bad port %q\n", portStr)
		return
	}

	for _, tc := range cfg.Tailnets {
		ts, lc, st, err := bringUpForQuery(ctx, tc, base)
		if err != nil {
			continue
		}
		suffix := suffixOf(st)
		if host != suffix && !strings.HasSuffix(host, "."+suffix) {
			ts.Close()
			continue
		}

		fmt.Printf("host:      %s\n", host)
		fmt.Printf("tailnet:   %s (%s)\n", tc.Name, suffix)

		ip, ok := newTailnetResolver(lc)(ctx, host)
		if !ok {
			fmt.Printf("resolve:   FAILED — not a known peer on this tailnet\n")
			fmt.Printf("           (run `peers %s` to see what exists)\n", tc.Name)
			ts.Close()
			return
		}
		fmt.Printf("resolve:   %s -> %s\n", host, ip)

		fmt.Printf("dial:      %s:%d over %s ...\n", ip, port, tc.Name)
		start := time.Now()
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := ts.Dial(dctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		elapsed := time.Since(start).Round(10 * time.Millisecond)
		cancel()
		if err != nil {
			fmt.Printf("result:    UNREACHABLE (%s) — %v\n", elapsed, err)
			ts.Close()
			return
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 96)
		n, _ := conn.Read(buf)
		conn.Close()
		ts.Close()
		if n > 0 {
			fmt.Printf("result:    OPEN (%s) — banner: %s\n", elapsed, printable(buf[:n]))
		} else {
			fmt.Printf("result:    OPEN (%s) — connected, no banner (server speaks first? try HTTP)\n", elapsed)
		}
		return
	}
	fmt.Printf("host:      %s\nresult:    no configured tailnet matches that suffix\n", host)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func printable(b []byte) string {
	s := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return -1
		}
		return r
	}, string(b))
	return strings.TrimSpace(s)
}

// parsePorts turns "22,80,443" into []int, skipping junk.
func parsePorts(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if p, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && p > 0 && p < 65536 {
			out = append(out, p)
		}
	}
	return out
}
