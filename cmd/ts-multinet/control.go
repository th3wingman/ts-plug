// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn/ipnstate"
)

// probeTimeout bounds each per-port reachability check.
const probeTimeout = 1500 * time.Millisecond

// Daemon is the running set of tailnets, queried by the control socket so the
// CLI never has to spin up its own tsnet stacks (which would fight for state
// locks, :53, and authkeys).
type Daemon struct {
	tailnets []*Tailnet
	reg      *registry
}

// --- JSON wire types (shared with the client) ---

type peerJSON struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	OS       string `json:"os"`
	Online   bool   `json:"online"`
	Services []int  `json:"services"`
}

type tailnetPeersJSON struct {
	Name   string     `json:"name"`
	Suffix string     `json:"suffix"`
	Up     int        `json:"up"`
	Peers  []peerJSON `json:"peers"`
}

type tailnetStatusJSON struct {
	Name       string `json:"name"`
	Suffix     string `json:"suffix"`
	CIDR       string `json:"cidr"`
	AssignedIP string `json:"assigned_ip"`
	Peers      int    `json:"peers"`
	Up         int    `json:"up"`
}

type checkJSON struct {
	Host       string `json:"host"`
	Tailnet    string `json:"tailnet"`
	Suffix     string `json:"suffix"`
	ResolvedIP string `json:"resolved_ip"`
	Result     string `json:"result"` // open | unreachable | resolve_failed | no_tailnet
	Detail     string `json:"detail"`
	LatencyMS  int64  `json:"latency_ms"`
	Banner     string `json:"banner"`
}

// serveControl runs the unix-socket control API until ctx is cancelled.
func (d *Daemon) serveControl(ctx context.Context, sockPath string) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0755); err != nil {
		slog.Error("control mkdir", "err", err)
		return
	}
	os.Remove(sockPath) // unlink stale socket
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		slog.Error("control listen", "path", sockPath, "err", err)
		return
	}
	os.Chmod(sockPath, 0660)

	mux := http.NewServeMux()
	mux.HandleFunc("/status", d.handleStatus)
	mux.HandleFunc("/peers", d.handlePeers)
	mux.HandleFunc("/check", d.handleCheck)
	srv := &http.Server{Handler: mux}
	go func() { <-ctx.Done(); srv.Close(); os.Remove(sockPath) }()

	slog.Info("control socket up", "path", sockPath)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		slog.Error("control serve", "err", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	out := make([]tailnetStatusJSON, 0, len(d.tailnets))
	for _, tn := range d.tailnets {
		ts := tailnetStatusJSON{Name: tn.conf.Name, Suffix: tn.suffix, CIDR: tn.conf.CIDR, AssignedIP: tn.assignedIP}
		if st, err := tn.lc.Status(r.Context()); err == nil {
			ts.Peers = len(st.Peer)
			for _, p := range st.Peer {
				if p.Online {
					ts.Up++
				}
			}
		}
		out = append(out, ts)
	}
	writeJSON(w, out)
}

func (d *Daemon) handlePeers(w http.ResponseWriter, r *http.Request) {
	filter := strings.ToLower(r.URL.Query().Get("filter"))
	ports := parsePorts(r.URL.Query().Get("ports"))
	out := make([]tailnetPeersJSON, 0, len(d.tailnets))

	for _, tn := range d.tailnets {
		tp := tailnetPeersJSON{Name: tn.conf.Name, Suffix: tn.suffix}
		st, err := tn.lc.Status(r.Context())
		if err != nil {
			out = append(out, tp)
			continue
		}
		var matched []*ipnstate.PeerStatus
		for _, p := range st.Peer {
			short := shortName(p.DNSName, tn.suffix)
			if filter != "" && !strings.Contains(short, filter) {
				continue
			}
			matched = append(matched, p)
			if p.Online {
				tp.Up++
			}
		}
		sort.Slice(matched, func(i, j int) bool { return matched[i].DNSName < matched[j].DNSName })

		services := probePeers(r.Context(), tn.ts.Dial, matched, ports)
		for _, p := range matched {
			ip := ""
			if len(p.TailscaleIPs) > 0 {
				ip = p.TailscaleIPs[0].String()
			}
			tp.Peers = append(tp.Peers, peerJSON{
				Name: shortName(p.DNSName, tn.suffix), IP: ip, OS: p.OS, Online: p.Online, Services: services[ip],
			})
		}
		out = append(out, tp)
	}
	writeJSON(w, out)
}

func (d *Daemon) handleCheck(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimSuffix(strings.ToLower(r.URL.Query().Get("host")), ".")
	port, _ := strconv.Atoi(r.URL.Query().Get("port"))
	if port == 0 {
		port = 80
	}
	res := checkJSON{Host: host}

	for _, tn := range d.tailnets {
		if host != tn.suffix && !strings.HasSuffix(host, "."+tn.suffix) {
			continue
		}
		res.Tailnet, res.Suffix = tn.conf.Name, tn.suffix
		ip, ok := tn.resolve(r.Context(), host)
		if !ok {
			res.Result = "resolve_failed"
			writeJSON(w, res)
			return
		}
		res.ResolvedIP = ip
		target := net.JoinHostPort(ip, strconv.Itoa(port))
		start := time.Now()
		dctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		conn, err := tn.ts.Dial(dctx, "tcp", target)
		res.LatencyMS = time.Since(start).Milliseconds()
		cancel()
		if err != nil {
			res.Result, res.Detail = "unreachable", err.Error()
			writeJSON(w, res)
			return
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		b := make([]byte, 96)
		n, _ := conn.Read(b)
		conn.Close()
		res.Result = "open"
		if n > 0 {
			res.Banner = printable(b[:n])
		}
		writeJSON(w, res)
		return
	}
	res.Result = "no_tailnet"
	writeJSON(w, res)
}

// probePeers dials each online peer's ports over the tailnet, returning the open
// ports per IP. Concurrency-limited so big tailnets don't fan out unboundedly.
func probePeers(ctx context.Context, dial dialFunc, peers []*ipnstate.PeerStatus, ports []int) map[string][]int {
	if len(ports) == 0 {
		return nil
	}
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
				if c, err := dial(dctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port))); err == nil {
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

func shortName(dnsName, suffix string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(dnsName), "."), "."+suffix)
}
