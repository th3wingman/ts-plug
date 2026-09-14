// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

	// cfgMu serializes config mutations so concurrent control requests
	// can't lose updates; d.mu still guards the tailnet list itself.
	cfgMu sync.Mutex

	// peerShorts lists a tailnet's selectable peer short names. A field so
	// control tests can stub the live tsnet status.
	peerShorts func(ctx context.Context, tn *Tailnet) ([]string, error)
}

func newDaemon(nets []*Tailnet, reg *registry, cfgPath, hostsFile string) *Daemon {
	return &Daemon{tailnets: nets, reg: reg, cfgPath: cfgPath, hostsFile: hostsFile, peerShorts: livePeerShorts}
}

// livePeerShorts is the production peerShorts: live status, Mullvad exits
// filtered out, names shortened to what `peers` and selection use.
func livePeerShorts(ctx context.Context, tn *Tailnet) ([]string, error) {
	st, err := tn.lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range st.Peer {
		if isMullvad(p.DNSName) {
			continue
		}
		if s := shortName(p.DNSName, tn.suffix); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
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
// confFor returns the current on-disk config for a tailnet, falling back to
// the daemon-start snapshot if the file is unreadable. loadConfig (not raw
// Unmarshal) so commented configs parse.
func (d *Daemon) confFor(name string) TailnetConf {
	if c, err := loadConfig(d.cfgPath); err == nil {
		if tc := confByName(c, name); tc != nil {
			return *tc
		}
	}
	for _, tn := range d.tailnets {
		if tn.conf.Name == name {
			return tn.conf
		}
	}
	return TailnetConf{Name: name}
}

// confByName finds a tailnet's conf by name.
func confByName(c *Config, name string) *TailnetConf {
	for i := range c.Tailnets {
		if c.Tailnets[i].Name == name {
			return &c.Tailnets[i]
		}
	}
	return nil
}

// cloneConfig deep-copies c through a JSON round trip — enough to diff
// against the patched result.
func cloneConfig(c *Config) (*Config, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// applyConfigUpdate is the one path by which the daemon edits its config: fn
// mutates a parsed copy, the file is rewritten comment-preserving (hujson
// AST), the in-memory confs are swapped, and selections re-apply (hosts
// block, synthetic IPs). Manual edits keep working — the file is re-read on
// every update; structural changes still need a restart.
func (d *Daemon) applyConfigUpdate(fn func(*Config) error) error {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	cfg, err := loadConfig(d.cfgPath)
	if err != nil {
		return err
	}
	prev, err := cloneConfig(cfg)
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		return err
	}
	if err := patchConfigFile(d.cfgPath, prev, cfg); err != nil {
		return err
	}
	d.mu.Lock()
	for _, tn := range d.tailnets {
		if tc := confByName(cfg, tn.conf.Name); tc != nil {
			tn.conf = *tc
		}
	}
	d.mu.Unlock()
	d.applySelections()
	return nil
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

func (d *Daemon) tailnetNames() []string {
	out := make([]string, 0, len(d.tailnets))
	for _, tn := range d.tailnets {
		out = append(out, tn.conf.Name)
	}
	return out
}

// controlMux builds the control API routes — shared by the unix socket and
// any other listener (web UI).
func (d *Daemon) controlMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", d.handleStatus)
	mux.HandleFunc("GET /peers", d.handlePeers)
	mux.HandleFunc("GET /check", d.handleCheck)
	mux.HandleFunc("GET /config", d.handleConfig)
	mux.HandleFunc("POST /login", d.handleLogin)
	mux.HandleFunc("POST /reload", d.handleReload)
	mux.HandleFunc("POST /tailnet/{name}/select", d.handleSelect)
	mux.HandleFunc("POST /tailnet/{name}/forget", d.handleForget)
	mux.HandleFunc("POST /tailnet/{name}/allow-all", d.handleAllowAll)
	mux.HandleFunc("POST /tailnet/{name}/domain", d.handleDomain)
	return mux
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

	srv := &http.Server{Handler: d.controlMux()}
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
	known := d.tailnetNames()
	d.mu.Unlock()
	if tn == nil {
		writeJSON(w, loginResult{Tailnet: req.Tailnet, Error: "no such tailnet (known: " + strings.Join(known, ", ") + ")"})
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

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]string{"error": msg})
}

// handleConfig returns the effective on-disk config — the file is the source
// of truth and is re-read on every query.
func (d *Daemon) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadConfig(d.cfgPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, cfg)
}

// selectionReq is the shared body of the mutation endpoints (each uses the
// field it needs; the rest must be absent or zero).
type selectionReq struct {
	Peer   string `json:"peer"`
	On     *bool  `json:"on"`
	Domain string `json:"domain"`
}

// clientError marks a request-level problem (unknown peer, bad domain) so
// updateTailnet maps it to 400 instead of 500.
type clientError struct{ msg string }

func (e clientError) Error() string { return e.msg }

// updateTailnet is the common flow of the /tailnet/{name}/* mutations:
// resolve the tailnet, list its selectable peers, hand the mutable conf to
// fn, and commit through applyConfigUpdate (comment-preserving write +
// re-apply). Peer and domain problems are rejected before anything is
// written.
func (d *Daemon) updateTailnet(w http.ResponseWriter, r *http.Request, fn func(req selectionReq, tc *TailnetConf, peers []string) error) {
	name := r.PathValue("name")
	var req selectionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	d.mu.Lock()
	tn := d.tailnetByName(name)
	known := d.tailnetNames()
	d.mu.Unlock()
	if tn == nil {
		writeErr(w, http.StatusNotFound, "no such tailnet (known: "+strings.Join(known, ", ")+")")
		return
	}
	peers, err := d.peerShorts(r.Context(), tn)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "tailnet status unavailable: "+err.Error())
		return
	}
	if err := d.applyConfigUpdate(func(c *Config) error {
		tc := confByName(c, name)
		if tc == nil {
			return fmt.Errorf("tailnet %q vanished from config", name)
		}
		return fn(req, tc, peers)
	}); err != nil {
		var ce clientError
		if errors.As(err, &ce) {
			writeErr(w, http.StatusBadRequest, ce.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "config updated; selections re-applied"})
}

// handleSelect adds a peer to the tailnet's resources. The peer must be a
// live, non-Mullvad peer — the same names `peers` and selection use.
func (d *Daemon) handleSelect(w http.ResponseWriter, r *http.Request) {
	d.updateTailnet(w, r, func(req selectionReq, tc *TailnetConf, peers []string) error {
		if req.Peer == "" {
			return clientError{"peer is required"}
		}
		if !slices.Contains(peers, req.Peer) {
			return clientError{fmt.Sprintf("unknown peer %q (peers: %s)", req.Peer, strings.Join(peers, ", "))}
		}
		if !slices.Contains(tc.Resources, req.Peer) {
			tc.Resources = append(tc.Resources, req.Peer)
		}
		return nil
	})
}

// handleForget removes a peer from resources (any name — stale entries can
// be cleaned up even when the peer is gone).
func (d *Daemon) handleForget(w http.ResponseWriter, r *http.Request) {
	d.updateTailnet(w, r, func(req selectionReq, tc *TailnetConf, peers []string) error {
		if req.Peer == "" {
			return clientError{"peer is required"}
		}
		tc.Resources = slices.DeleteFunc(tc.Resources, func(s string) bool { return s == req.Peer })
		return nil
	})
}

// handleAllowAll toggles selecting every non-Mullvad peer.
func (d *Daemon) handleAllowAll(w http.ResponseWriter, r *http.Request) {
	d.updateTailnet(w, r, func(req selectionReq, tc *TailnetConf, peers []string) error {
		if req.On == nil {
			return clientError{"body must be {\"on\": true|false}"}
		}
		tc.AllowAll = *req.On
		return nil
	})
}

// handleDomain sets the tailnet's friendly DNS suffix (my-server.<domain>);
// an empty string removes the override, falling back to the tailnet name.
func (d *Daemon) handleDomain(w http.ResponseWriter, r *http.Request) {
	d.updateTailnet(w, r, func(req selectionReq, tc *TailnetConf, peers []string) error {
		if req.Domain != "" && !validDomain(req.Domain) {
			return clientError{fmt.Sprintf("invalid domain %q: lowercase labels of [a-z0-9-], 1-63 chars each, 253 total", req.Domain)}
		}
		tc.Domain = req.Domain
		return nil
	})
}

// domainRE matches a lowercase DNS name: labels of 1-63 [a-z0-9-] that
// neither start nor end with a hyphen, dot-separated.
var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func validDomain(s string) bool {
	return len(s) <= 253 && domainRE.MatchString(s)
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
