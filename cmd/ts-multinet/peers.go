// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// runPeers brings each tailnet up just far enough to list its peers, then
// exits. Handy for finding reachable hostnames to point traffic at. It does NOT
// create TUNs or forwarders, so it needs no extra capabilities — run it in a
// throwaway container so it doesn't fight the main process for state locks.
func runPeers(ctx context.Context, cfg *Config) {
	base := orDefault(cfg.StateDir, ".state")
	for _, tc := range cfg.Tailnets {
		authkey := os.Getenv(tc.AuthKeyEnv)
		if authkey == "" {
			fmt.Printf("\n== %s == (skipped: %s is empty)\n", tc.Name, tc.AuthKeyEnv)
			continue
		}
		dir := tc.StateDir
		if dir == "" {
			dir = filepath.Join(base, tc.Name)
		}
		ts := &tsnet.Server{
			Hostname:  "ts-multinet-" + tc.Name,
			Dir:       dir,
			AuthKey:   authkey,
			Ephemeral: true,
		}
		if _, err := ts.Up(ctx); err != nil {
			fmt.Printf("\n== %s == (error: %v)\n", tc.Name, err)
			ts.Close()
			continue
		}
		lc, err := ts.LocalClient()
		if err != nil {
			fmt.Printf("\n== %s == (local client error: %v)\n", tc.Name, err)
			ts.Close()
			continue
		}
		st, err := lc.Status(ctx)
		if err != nil {
			fmt.Printf("\n== %s == (status error: %v)\n", tc.Name, err)
			ts.Close()
			continue
		}

		suffix := st.MagicDNSSuffix
		if st.CurrentTailnet != nil && st.CurrentTailnet.MagicDNSSuffix != "" {
			suffix = st.CurrentTailnet.MagicDNSSuffix
		}
		fmt.Printf("\n== %s (%s) ==\n", tc.Name, suffix)

		peers := make([]*ipnstate.PeerStatus, 0, len(st.Peer))
		for _, p := range st.Peer {
			peers = append(peers, p)
		}
		sort.Slice(peers, func(i, j int) bool {
			return peers[i].DNSName < peers[j].DNSName
		})
		fmt.Printf("  %-5s %-40s %-16s %s\n", "STATE", "DNSNAME", "IP", "OS")
		for _, p := range peers {
			state := "down"
			if p.Online {
				state = "UP"
			}
			ip := ""
			if len(p.TailscaleIPs) > 0 {
				ip = p.TailscaleIPs[0].String()
			}
			fmt.Printf("  %-5s %-40s %-16s %s\n", state, strings.TrimSuffix(p.DNSName, "."), ip, p.OS)
		}
		ts.Close()
	}
}
