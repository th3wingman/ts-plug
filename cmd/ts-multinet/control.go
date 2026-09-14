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
	mu        sync.Mutex
	tailnets  []*Tailnet
	reg       *registry
	cfgPath   string
	hostsFile string
}

func newDaemon(nets []*Tailnet, reg *registry, cfgPath, hostsFile string) *Daemon {
	return &Daemon{tailnets: nets, reg: reg, cfgPath: cfgPath, hostsFile: hostsFile}
}

func (d *Daemon) setTailnets(nets []*Tailnet) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tailnets = nets
}

// applySelections resolves every running tailnet's configured selections,
// seeds deterministic synthetic IPs, and rewrites the /etc/hosts managed
// block. Called on each tailnet's transition to Running and on reload. A
// per-tailnet watcher firing at the same time just re-runs the same
// idempotent rewrite.
func (d *Daemon) applySelections() {
	d.mu.Lock()
	defer d.mu.Unlock()

	var entries []hostsEntry
	for _, tn := range d.tailnets {
		state, _ := tn.status()
		if state != "Running" {
			tn.setApplied(0)
			continue
		}
		st, err := tn.lc.Status(context.Background())
		if err != nil {
			slog.Warn("selection status failed", "name", tn.conf.Name, "err", err)
			continue
		}
		pins, err := loadPins(tn.stateDir)
		if err != nil {
			slog.Error("selection pins unreadable", "name", tn.conf.Name, "err", err)
			continue
		}
		conf := d.confFor(tn.conf.Name)
		resolved, errs, changed := resolveSelections(tn.suffix, conf, st, pins)
		for _, e := range errs {
			slog.Error("selection skipped", "name", tn.conf.Name, "resource", e)
		}
		if changed {
			if err := savePins(tn.stateDir, pins); err != nil {
				slog.Error("selection pins unwritable", "name", tn.conf.Name, "err", err)
			}
		}
		// Seed in sorted-FQDN order so synthetic IPs are stable across restarts.
		fqdns := make([]string, 0, len(resolved))
		for _, r := range resolved {
			fqdns = append(fqdns, r.FQDN)
		}
		sort.Strings(fqdns)
		d.reg.seed(conf.Name, fqdns)
		applied := 0
		for _, r := range resolved {
			ip, ok := d.reg.allocate(conf.Name, r.FQDN)
			if !ok {
				slog.Error("synthetic range exhausted", "name", conf.Name, "resource", r.Short)
				continue
			}
			entries = append(entries, hostsEntry{
				IP:      ip.String(),
				Alias:   r.Short + "." + conf.Name,
				FQDN:    r.FQDN,
				Tailnet: conf.Name,
			})
			applied++
		}
		tn.setApplied(applied)
	}
	if err := writeHostsBlock(d.hostsFile, entries); err != nil {
		slog.Error("hosts block rewrite failed", "path", d.hostsFile, "err", err)
	} else {
		slog.Info("hosts block updated", "path", d.hostsFile, "entries", len(entries))
	}
}

// confFor returns the current on-disk config for a tailnet, falling back to
// the daemon-start snapshot if the file is unreadable.
func (d *Daemon) confFor(name string) TailnetConf {
	b, err := os.ReadFile(d.cfgPath)
	if err == nil {
		var c Config
		if json.Unmarshal(b, &c) == nil {
			for _, tc := range c.Tailnets {
				if tc.Name == name {
					return tc
				}
			}
		}
	}
	for _, tn := range d.tailnets {
		if tn.conf.Name == name {
			return tn.conf
		}
	}
	return TailnetConf{Name: name}
}

// reload re-applies selections from the current config file and rewrites the
// hosts block. Structural changes (adding/removing tailnets, changing cidr or
// tun) need a daemon restart; selection changes apply immediately.
func (d *Daemon) reload() string {
	d.applySelections()
	return "selections re-applied; structural tailnet changes need a restart"
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
	State      string `json:"state"` // NeedsLogin, Running, …
	LoginURL   string `json:"login_url,omitempty"`
	Selected   int    `json:"selected"` // selections currently in the hosts block
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
	mux.HandleFunc("GET /status", d.handleStatus)
	mux.HandleFunc("GET /peers", d.handlePeers)
	mux.HandleFunc("GET /check", d.handleCheck)
	mux.HandleFunc("POST /login", d.handleLogin)
	mux.HandleFunc("POST /reload", d.handleReload)
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
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]tailnetStatusJSON, 0, len(d.tailnets))
	for _, tn := range d.tailnets {
		state, loginURL := tn.status()
		ts := tailnetStatusJSON{Name: tn.conf.Name, Suffix: tn.suffix, CIDR: tn.conf.CIDR, AssignedIP: tn.assignedIP, State: state, LoginURL: loginURL, Selected: tn.selectedCount()}
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
			if isMullvad(p.DNSName) {
				continue // shared transit infrastructure, not a resource
			}
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

	// locate handles fqdn, friendly alias (host.skynet), and bare names
	// (tried against each tailnet) — same resolution `ping` gets via search.
	tailnet, realFQDN, ok := d.reg.locate(r.Context(), host)
	if !ok {
		res.Result = "resolve_failed"
		writeJSON(w, res)
		return
	}
	tn := d.tailnetByName(tailnet)
	if tn == nil {
		res.Result = "no_tailnet"
		writeJSON(w, res)
		return
	}
	res.Tailnet, res.Suffix = tn.conf.Name, tn.suffix
	ip, ok := tn.resolve(r.Context(), realFQDN)
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
}

func (d *Daemon) tailnetByName(name string) *Tailnet {
	for _, tn := range d.tailnets {
		if strings.EqualFold(tn.conf.Name, name) {
			return tn
		}
	}
	return nil
}

// loginResult is the /login response: either a login URL to open in a
// browser, or the current state when no action is needed.
type loginResult struct {
	Tailnet  string `json:"tailnet"`
	State    string `json:"state"`
	LoginURL string `json:"login_url,omitempty"`
	Error    string `json:"error,omitempty"`
}

// handleLogin starts (or reports on) browser login for one tailnet. Persistent
// state means this is a one-time action per tailnet; later daemon starts come
// up Running on their own.
func (d *Daemon) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tailnet string `json:"tailnet"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Tailnet == "" {
		writeJSON(w, loginResult{Error: "tailnet is required"})
		return
	}
	d.mu.Lock()
	tn := d.tailnetByName(req.Tailnet)
	d.mu.Unlock()
	if tn == nil {
		writeJSON(w, loginResult{Tailnet: req.Tailnet, Error: "no such tailnet"})
		return
	}
	state, _ := tn.status()
	if state == "Running" {
		writeJSON(w, loginResult{Tailnet: req.Tailnet, State: state})
		return
	}
	if err := tn.lc.StartLoginInteractive(r.Context()); err != nil {
		writeJSON(w, loginResult{Tailnet: req.Tailnet, State: state, Error: err.Error()})
		return
	}
	// Poll for the auth URL to surface (usually immediate).
	loginURL := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := tn.lc.Status(r.Context())
		if err == nil && st.AuthURL != "" {
			loginURL = st.AuthURL
			break
		}
		if r.Context().Err() != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	state, _ = tn.status()
	res := loginResult{Tailnet: req.Tailnet, State: state}
	if loginURL != "" {
		res.LoginURL = loginURL
	} else {
		res.Error = "login started but no URL surfaced; check `status`"
	}
	writeJSON(w, res)
}

// handleReload re-applies selections from the config file and rewrites the
// hosts block. Structural changes need a daemon restart.
func (d *Daemon) handleReload(w http.ResponseWriter, r *http.Request) {
	msg := d.reload()
	writeJSON(w, map[string]string{"ok": msg})
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
