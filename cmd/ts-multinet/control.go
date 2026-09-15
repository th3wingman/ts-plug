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

	// rt is what syncTailnets needs to start tailnets at runtime. Startup
	// uses the same path, so a boot-time add and an API add are identical.
	// starter is swappable so tests can watch starts/stops without tsnet.
	rt struct {
		ctx        context.Context
		mtu        uint32
		baseDir    string
		rs         *resolvedSync
		starter    func(ctx context.Context, conf TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error)
		cleanupTUN func(cidr, dev string) // stopTailnet device cleanup; swappable in tests
	}

	// cfgMu serializes config mutations so concurrent control requests
	// can't lose updates; d.mu still guards the tailnet list itself.
	cfgMu sync.Mutex

	// tunRetries marks tailnet names with a busy-TUN background retry in
	// flight, so a second spawn is a no-op. Guarded by d.mu.
	tunRetries map[string]bool

	// peerShorts lists a tailnet's selectable peer short names. A field so
	// control tests can stub the live tsnet status.
	peerShorts func(ctx context.Context, tn *Tailnet) ([]string, error)
}

func newDaemon(nets []*Tailnet, reg *registry, cfgPath, hostsFile string) *Daemon {
	return &Daemon{tailnets: nets, reg: reg, cfgPath: cfgPath, hostsFile: hostsFile, peerShorts: livePeerShorts, tunRetries: map[string]bool{}}
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

// liveTailnets returns a copy of the running set.
func (d *Daemon) liveTailnets() []*Tailnet {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*Tailnet(nil), d.tailnets...)
}

// syncTailnets makes the running set match cfg: added tailnets start,
// removed or disabled ones stop, a cidr/tun change is stop+start, and
// everything else (resources, allow_all, domain) swaps in place. Startup and
// every config mutation funnel through here, so nothing needs a service
// restart. Tailnets left in the config that fail to start are reported as an
// error and retried on the next sync — never dropped from the file.
func (d *Daemon) syncTailnets(cfg *Config) error {
	if d.rt.starter == nil {
		d.rt.starter = startTailnet
	}
	if d.rt.cleanupTUN == nil {
		d.rt.cleanupTUN = cleanupTUNImpl
	}

	wantByName := make(map[string]*TailnetConf, len(cfg.Tailnets))
	for i := range cfg.Tailnets {
		wantByName[strings.ToLower(cfg.Tailnets[i].Name)] = &cfg.Tailnets[i]
	}

	// Stop pass: gone from config, disabled, or structurally changed.
	for _, tn := range d.liveTailnets() {
		tc, want := wantByName[strings.ToLower(tn.conf.Name)]
		if !want || !tc.enabled() || tn.conf.CIDR != tc.CIDR || tn.conf.TUN != tc.TUN || tn.conf.Hostname != tc.Hostname {
			d.stopTailnet(tn)
			continue
		}
		// Mutable swap. The registry must follow a domain change here or DNS
		// would keep matching the old friendly suffix (the watcher only
		// re-registers the resolved side).
		if tn.conf.Domain != tc.Domain {
			d.reg.registerDomain(tc.Name, tc.domainName())
		}
		tn.conf = *tc
	}

	// Start pass: enabled entries not yet running (added, restarted after a
	// cidr/tun change, or failed earlier and retried).
	live := make(map[string]bool)
	for _, tn := range d.liveTailnets() {
		live[strings.ToLower(tn.conf.Name)] = true
	}
	var startErrs []string
	for _, tc := range cfg.Tailnets {
		if !tc.enabled() || live[strings.ToLower(tc.Name)] {
			continue
		}
		if err := d.startOne(tc); err != nil {
			startErrs = append(startErrs, fmt.Sprintf("%s: %v", tc.Name, err))
			if isTUNBusy(err) {
				// the kernel can need tens of seconds to release the name
				// (deferred device teardown) — retry patiently in the background
				d.spawnTUNRetry(tc)
			}
			continue
		}
	}
	d.applySelections()
	if len(startErrs) > 0 {
		return fmt.Errorf("tailnets in config but not started (retried on every config change and `reload`): %s", strings.Join(startErrs, "; "))
	}
	return nil
}

// isTUNBusy reports whether a start error is the kernel holding the tun
// name — the only failure safe to blindly retry (openTUN fails before any
// resource exists).
func isTUNBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "device or resource busy")
}

// startOne starts one tailnet (quick busy retries included) and adds it to
// the running set. Shared by syncTailnets and the background busy retry.
func (d *Daemon) startOne(tc TailnetConf) error {
	if err := d.reg.add(tc); err != nil {
		return err
	}
	slog.Info("tailnet starting", "name", tc.Name, "tun", tc.TUN, "cidr", tc.CIDR)
	tn, err := startWithTUNRetry(d.rt.starter, d.rt.ctx, tc, d.reg, d.rt.mtu, d.rt.baseDir, d.applySelections, d.rt.rs)
	if err != nil {
		d.reg.remove(tc.Name)
		return err
	}
	d.mu.Lock()
	d.tailnets = append(d.tailnets, tn)
	d.mu.Unlock()
	return nil
}

// tunRetryBackoff is the schedule a parked busy-TUN start retries on after
// the quick in-sync attempts. Swappable in tests for a fast schedule.
var tunRetryBackoff = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 45 * time.Second}

// spawnTUNRetry retries a busy-TUN start in the background: one goroutine
// per tailnet name (a second spawn while one is pending is a no-op), on
// tunRetryBackoff, until it starts, the tailnet leaves the config, or the
// daemon shuts down.
func (d *Daemon) spawnTUNRetry(tc TailnetConf) {
	d.mu.Lock()
	if d.tunRetries[tc.Name] {
		d.mu.Unlock()
		return
	}
	d.tunRetries[tc.Name] = true
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.tunRetries, tc.Name)
			d.mu.Unlock()
		}()
		for _, wait := range tunRetryBackoff {
			select {
			case <-d.rt.ctx.Done():
				return
			case <-time.After(wait):
			}
			if !d.tailnetInConfig(tc.Name) {
				return // removed from config while we waited
			}
			d.mu.Lock()
			running := d.tailnetByName(tc.Name) != nil
			d.mu.Unlock()
			if running {
				return // a sync started it meanwhile
			}
			if err := d.startOne(tc); err == nil {
				d.applySelections()
				slog.Info("tailnet started after busy retry", "name", tc.Name, "tun", tc.TUN)
				return
			} else {
				slog.Warn("tun name still busy, background retry scheduled", "name", tc.Name, "tun", tc.TUN, "err", err)
			}
		}
		slog.Warn("tun name stayed busy; retried again on the next config change or `reload`", "name", tc.Name, "tun", tc.TUN)
	}()
}

// startWithTUNRetry retries a start whose TUN name is briefly held: the
// predecessor process during a systemd restart (or a just-stopped sibling in
// the same sync) can keep the interface for a moment while it unwinds.
// Retrying only the busy error is safe — openTUN fails before any resource
// exists, so a re-attempt has no side effects to clean up.
func startWithTUNRetry(starter func(context.Context, TailnetConf, *registry, uint32, string, func(), *resolvedSync) (*Tailnet, error), ctx context.Context, tc TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
	for try := 0; ; try++ {
		tn, err := starter(ctx, tc, reg, mtu, baseDir, onRunning, rs)
		if err == nil || try >= 2 || !strings.Contains(err.Error(), "device or resource busy") {
			return tn, err
		}
		slog.Warn("tun name busy, retrying", "name", tc.Name, "tun", tc.TUN, "attempt", try+1)
		time.Sleep(500 * time.Millisecond)
	}
}

// stopTailnet tears one tailnet down: out of the list, resolved reverted
// while its TUN still exists, registry scrubbed, then goroutines + TUN +
// tsnet. Node state (login identity, selection pins) is kept — a re-add
// comes back up Running without a new browser login.
func (d *Daemon) stopTailnet(tn *Tailnet) {
	d.mu.Lock()
	d.tailnets = slices.DeleteFunc(d.tailnets, func(t *Tailnet) bool { return t == tn })
	d.mu.Unlock()
	if tn.resolved != nil {
		tn.resolved.revert(tn.dev)
	}
	d.reg.remove(tn.conf.Name)
	// Drop the route and address we put on the device before its fd closes:
	// the kernel's deferred teardown can wait on them and keep the name busy
	// (TUNSETIFF EBUSY) for a stop/start of the same tailnet.
	if d.rt.cleanupTUN != nil {
		d.rt.cleanupTUN(tn.conf.CIDR, tn.dev)
	}
	tn.Close()
	slog.Info("tailnet stopped", "name", tn.conf.Name, "tun", tn.dev)
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
				Alias:   r.Short + "." + conf.domainName(),
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

// tailnetInConfig reports whether name exists in the config file. Such a
// tailnet may still not be running (parked start) — distinct from unknown.
func (d *Daemon) tailnetInConfig(name string) bool {
	c, err := loadConfig(d.cfgPath)
	return err == nil && confByName(c, name) != nil
}

// confByName finds a tailnet's conf by name.
func confByName(c *Config, name string) *TailnetConf {
	for i := range c.Tailnets {
		if strings.EqualFold(c.Tailnets[i].Name, name) {
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
// AST), and syncTailnets makes the running set match (starts/stops included).
// Returns a notice naming global fields fn changed that only a service
// restart can apply. Manual edits keep working — the file is re-read on every
// update and `reload` syncs them live.
func (d *Daemon) applyConfigUpdate(fn func(*Config) error) (string, error) {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	cfg, err := loadConfig(d.cfgPath)
	if err != nil {
		return "", err
	}
	prev, err := cloneConfig(cfg)
	if err != nil {
		return "", err
	}
	if err := fn(cfg); err != nil {
		return "", err
	}
	if err := patchConfigFile(d.cfgPath, prev, cfg); err != nil {
		return "", err
	}
	notice := restartNotice(prev, cfg)
	if serr := d.syncTailnets(cfg); serr != nil {
		// the requested change IS written; a start failure just parks the
		// tailnet (retried on the next change, `reload`, or the background
		// busy retry) — report it as a notice, not a request failure
		notice = strings.TrimSpace(notice + "; " + serr.Error())
	}
	return notice, nil
}

// restartNotice names the global (non-tailnet) fields that changed and need
// a service restart — everything tailnet-level applies live via syncTailnets.
func restartNotice(prev, next *Config) string {
	var fields []string
	if prev.MTU != next.MTU {
		fields = append(fields, "mtu")
	}
	if prev.DNSListen != next.DNSListen {
		fields = append(fields, "dns_listen")
	}
	if prev.UpstreamDNS != next.UpstreamDNS {
		fields = append(fields, "upstream_dns")
	}
	if prev.StateDir != next.StateDir {
		fields = append(fields, "state_dir")
	}
	if prev.HostsFile != next.HostsFile {
		fields = append(fields, "hosts_file")
	}
	if prev.UIListen != next.UIListen {
		fields = append(fields, "ui_listen")
	}
	if len(fields) == 0 {
		return ""
	}
	return "restart needed for: " + strings.Join(fields, ", ")
}

// reload re-reads the config file and syncs the running tailnets to it, so
// manual edits (tailnets included) apply without a service restart. Only
// globals (mtu, dns_listen, upstream_dns, state_dir, hosts_file, ui_listen)
// still need one.
func (d *Daemon) reload() string {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	cfg, err := loadConfig(d.cfgPath)
	if err != nil {
		return "config unreadable: " + err.Error()
	}
	if err := d.syncTailnets(cfg); err != nil {
		return err.Error()
	}
	return "config re-read and applied (tailnets started/stopped as needed); globals (mtu, dns_listen, upstream_dns, state_dir, hosts_file, ui_listen) still need a restart"
}

// --- JSON wire types (shared with the client) ---

type peerJSON struct {
	Name     string `json:"name"`
	FQDN     string `json:"fqdn,omitempty"` // full MagicDNS name (peers --details)
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
	Hostname   string `json:"hostname"` // node name reported inside the tailnet
	AssignedIP string `json:"assigned_ip"`
	State      string `json:"state"` // NeedsLogin, Running, …
	LoginURL   string `json:"login_url,omitempty"`
	Selected   int    `json:"selected"` // selections currently in the hosts block
	Peers      int    `json:"peers"`
	Up         int    `json:"up"`

	// DNSRegistered is true when this tailnet's systemd-resolved routing
	// domains are applied (host DNS reaches our responder). Always false in
	// containers: resolv.conf is ours there.
	DNSRegistered bool `json:"dns_registered"`
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
	mux.HandleFunc("POST /config", d.handleUpdateConfig)
	mux.HandleFunc("POST /login", d.handleLogin)
	mux.HandleFunc("POST /reload", d.handleReload)
	mux.HandleFunc("POST /tailnet/{name}/select", d.handleSelect)
	mux.HandleFunc("POST /tailnet/{name}/forget", d.handleForget)
	mux.HandleFunc("POST /tailnet/{name}/allow-all", d.handleAllowAll)
	mux.HandleFunc("POST /tailnet/{name}/domain", d.handleDomain)
	mux.HandleFunc("POST /tailnet/{name}/hostname", d.handleHostname)
	mux.HandleFunc("POST /tailnet", d.handleAddTailnet)
	mux.HandleFunc("DELETE /tailnet/{name}", d.handleRemoveTailnet)
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
		ts := tailnetStatusJSON{Name: tn.conf.Name, Suffix: tn.suffix, CIDR: tn.conf.CIDR, Hostname: tn.conf.nodeHostname(), AssignedIP: tn.assignedIP, State: state, LoginURL: loginURL, Selected: tn.selectedCount()}
		ts.DNSRegistered = tn.resolved.isRegistered(tn.dev)
		// lc is nil only in tests: stubbed tailnets have no tsnet client.
		if tn.lc != nil {
			if st, err := tn.lc.Status(r.Context()); err == nil {
				for _, p := range st.Peer {
					if isMullvad(p.DNSName) {
						continue // shared transit, not resources — same filter as `peers`
					}
					ts.Peers++
					if p.Online {
						ts.Up++
					}
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
	// Snapshot the running set under the lock — sync/stop mutate it concurrently
	// (a stop mid-poll used to race this loop). The Tailnet pointers stay valid;
	// status calls on a stopped one just return errors and are handled below.
	d.mu.Lock()
	tailnets := append([]*Tailnet(nil), d.tailnets...)
	d.mu.Unlock()
	out := make([]tailnetPeersJSON, 0, len(tailnets))

	for _, tn := range tailnets {
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
				Name: shortName(p.DNSName, tn.suffix), FQDN: strings.TrimSuffix(p.DNSName, "."), IP: ip, OS: p.OS, Online: p.Online, Services: services[ip],
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
		msg := "no such tailnet (known: " + strings.Join(known, ", ") + ")"
		if d.tailnetInConfig(req.Tailnet) {
			msg = "tailnet is in the config but not running — `ts-multinet reload` retries it"
		}
		writeJSON(w, loginResult{Tailnet: req.Tailnet, Error: msg})
		return
	}
	state, pendingURL := tn.status()
	if state == "Running" {
		writeJSON(w, loginResult{Tailnet: req.Tailnet, State: state})
		return
	}
	// A login is already pending: re-triggering would invalidate that URL
	// (and any browser tab mid-auth on it). The backend drops AuthURL from
	// status once the session expires, so an empty URL still gets a fresh one.
	if pendingURL != "" {
		writeJSON(w, loginResult{Tailnet: req.Tailnet, State: state, LoginURL: pendingURL})
		return
	}
	if err := tn.lc.StartLoginInteractive(r.Context()); err != nil {
		writeJSON(w, loginResult{Tailnet: req.Tailnet, State: state, Error: err.Error()})
		return
	}
	// Poll for the auth URL to surface (usually immediate). Fresh nodes can
	// take longer than one request should hold open, so the window is modest
	// and the watcher keeps recording the URL for status/UI to pick up.
	loginURL := ""
	deadline := time.Now().Add(20 * time.Second)
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
	state, recorded := tn.status() // the watcher may have caught it after the poll gave up
	res := loginResult{Tailnet: req.Tailnet, State: state}
	switch {
	case loginURL != "":
		res.LoginURL = loginURL
	case recorded != "":
		res.LoginURL = recorded
	default:
		res.Error = "login started; the URL will appear in `status` and on the dashboard shortly"
	}
	writeJSON(w, res)
}

// handleReload re-reads the config file and syncs the running tailnets to
// it — manual edits (tailnets included) apply without a service restart.
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

// minMTU/maxMTU keep mtu edits inside what the datapath can actually carry:
// WireGuard needs at least 1280, and 9000 is the practical ceiling.
const (
	minMTU = 1280
	maxMTU = 9000
)

// globalsReq is the POST /config body: the daemon-wide settings only. Every
// field is optional and pointer-typed so "absent" (untouched) is distinct
// from "present but empty" (clear the override, fall back to the default).
// state_dir is accepted only so it can be rejected loudly.
type globalsReq struct {
	MTU         *int    `json:"mtu"`
	DNSListen   *string `json:"dns_listen"`
	UpstreamDNS *string `json:"upstream_dns"`
	UIListen    *string `json:"ui_listen"`
	HostsFile   *string `json:"hosts_file"`
	StateDir    *string `json:"state_dir"`
}

// handleUpdateConfig sets the daemon-wide globals. The config file stays the
// source of truth (patched comment-preserving); these fields only take effect
// at the next service restart, so the reply names the ones that changed in
// needs_restart for the UI to badge.
func (d *Daemon) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var req globalsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.StateDir != nil {
		writeErr(w, http.StatusBadRequest, "state_dir is manual-only — moving it orphans node state; edit the config file directly and restart")
		return
	}
	if req.MTU != nil && *req.MTU != 0 && (*req.MTU < minMTU || *req.MTU > maxMTU) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("mtu must be %d..%d (0 resets to the default)", minMTU, maxMTU))
		return
	}
	if req.DNSListen != nil && *req.DNSListen != "" && !validListenAddr(*req.DNSListen) {
		writeErr(w, http.StatusBadRequest, "dns_listen must be host:port")
		return
	}
	if req.UIListen != nil && *req.UIListen != "" && !validListenAddr(*req.UIListen) {
		writeErr(w, http.StatusBadRequest, "ui_listen must be host:port")
		return
	}
	if req.UpstreamDNS != nil && *req.UpstreamDNS != "" && !validUpstreamDNS(*req.UpstreamDNS) {
		writeErr(w, http.StatusBadRequest, "upstream_dns must be an IP or host:port")
		return
	}
	if req.HostsFile != nil && *req.HostsFile != "" && !filepath.IsAbs(*req.HostsFile) {
		writeErr(w, http.StatusBadRequest, "hosts_file must be an absolute path")
		return
	}

	// changed is the request-order list of globals that actually moved (the
	// config-file diff uses the same six fields for its restart notice).
	var changed []string
	notice, err := d.applyConfigUpdate(func(c *Config) error {
		if setInt(&c.MTU, req.MTU) {
			changed = append(changed, "mtu")
		}
		if setStr(&c.DNSListen, req.DNSListen) {
			changed = append(changed, "dns_listen")
		}
		if setStr(&c.UpstreamDNS, req.UpstreamDNS) {
			changed = append(changed, "upstream_dns")
		}
		if setStr(&c.UIListen, req.UIListen) {
			changed = append(changed, "ui_listen")
		}
		if setStr(&c.HostsFile, req.HostsFile) {
			changed = append(changed, "hosts_file")
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok := "config updated"
	if notice != "" {
		ok += "; " + notice
	}
	writeJSON(w, map[string]string{"ok": ok, "needs_restart": strings.Join(changed, ", ")})
}

// setInt applies an optional numeric global, reporting whether it changed.
func setInt(dst *int, v *int) bool {
	if v == nil || *dst == *v {
		return false
	}
	*dst = *v
	return true
}

// setStr applies an optional string global, reporting whether it changed.
func setStr(dst *string, v *string) bool {
	if v == nil || *dst == *v {
		return false
	}
	*dst = *v
	return true
}

// validListenAddr accepts host:port. An empty host is rejected — 0.0.0.0 is
// the explicit way to bind every interface.
func validListenAddr(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536
}

// validUpstreamDNS accepts a bare IP or host:port; the daemon's ensurePort
// adds the default DNS port for the bare form at startup.
func validUpstreamDNS(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	return validListenAddr(s)
}

// selectionReq is the shared body of the mutation endpoints (each uses the
// field it needs; the rest must be absent or zero).
type selectionReq struct {
	Peer     string `json:"peer"`
	On       *bool  `json:"on"`
	Domain   string `json:"domain"`
	Hostname string `json:"hostname"`
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
// updateTailnet is the shared body of the per-tailnet mutation endpoints.
// needsLive marks handlers that validate against the live peer list
// (select/forget): those still 404 on a parked tailnet. The config-only
// handlers (domain/hostname/allow-all) proceed on a parked one — the config
// entry is mutated and the next sync (or background busy retry) starts it.
func (d *Daemon) updateTailnet(w http.ResponseWriter, r *http.Request, needsLive bool, fn func(req selectionReq, tc *TailnetConf, peers []string) error, note func(req selectionReq) string) {
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
	var peers []string
	if tn == nil {
		inConfig := d.tailnetInConfig(name)
		if needsLive || !inConfig {
			msg := "no such tailnet (known: " + strings.Join(known, ", ") + ")"
			if inConfig {
				msg = "tailnet is in the config but not running — `ts-multinet reload` retries it"
			}
			writeErr(w, http.StatusNotFound, msg)
			return
		}
		// parked, config-only mutation: peers stay nil; these handlers never
		// look at them
	} else {
		var err error
		if peers, err = d.peerShorts(r.Context(), tn); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "tailnet status unavailable: "+err.Error())
			return
		}
	}
	notice, err := d.applyConfigUpdate(func(c *Config) error {
		tc := confByName(c, name)
		if tc == nil {
			return fmt.Errorf("tailnet %q vanished from config", name)
		}
		return fn(req, tc, peers)
	})
	if err != nil {
		var ce clientError
		if errors.As(err, &ce) {
			writeErr(w, http.StatusBadRequest, ce.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok := "config updated; selections re-applied"
	if note != nil {
		if extra := note(req); extra != "" {
			ok += "; " + extra
		}
	}
	if notice != "" {
		ok += "; " + notice
	}
	writeJSON(w, map[string]string{"ok": ok})
}

// handleSelect adds a peer to the tailnet's resources. The peer must be a
// live, non-Mullvad peer — the same names `peers` and selection use.
func (d *Daemon) handleSelect(w http.ResponseWriter, r *http.Request) {
	d.updateTailnet(w, r, true, func(req selectionReq, tc *TailnetConf, peers []string) error {
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
	}, nil)
}

// handleForget removes a peer from resources (any name — stale entries can
// be cleaned up even when the peer is gone).
func (d *Daemon) handleForget(w http.ResponseWriter, r *http.Request) {
	d.updateTailnet(w, r, true, func(req selectionReq, tc *TailnetConf, peers []string) error {
		if req.Peer == "" {
			return clientError{"peer is required"}
		}
		tc.Resources = slices.DeleteFunc(tc.Resources, func(s string) bool { return s == req.Peer })
		return nil
	}, nil)
}

// handleAllowAll toggles selecting every non-Mullvad peer.
func (d *Daemon) handleAllowAll(w http.ResponseWriter, r *http.Request) {
	d.updateTailnet(w, r, false, func(req selectionReq, tc *TailnetConf, peers []string) error {
		if req.On == nil {
			return clientError{"body must be {\"on\": true|false}"}
		}
		tc.AllowAll = *req.On
		return nil
	}, nil)
}

// handleDomain sets the tailnet's friendly DNS suffix (my-server.<domain>);
// an empty string removes the override, falling back to the tailnet name.
func (d *Daemon) handleDomain(w http.ResponseWriter, r *http.Request) {
	d.updateTailnet(w, r, false, func(req selectionReq, tc *TailnetConf, peers []string) error {
		if req.Domain != "" && !validDomain(req.Domain) {
			return clientError{fmt.Sprintf("invalid domain %q: lowercase labels of [a-z0-9-], 1-63 chars each, 253 total", req.Domain)}
		}
		tc.Domain = req.Domain
		return nil
	}, func(req selectionReq) string {
		// cleared domains fall back to the (possibly short) name — warn on that too
		domain := orDefault(req.Domain, slugify(strings.SplitN(r.PathValue("name"), ".", 2)[0]))
		return tldWarning(domain)
	})
}

// handleHostname sets the node name reported inside the tailnet (default
// ts-multinet-<slug>). Applying it restarts just that tailnet — node state
// persists, so no new browser login.
func (d *Daemon) handleHostname(w http.ResponseWriter, r *http.Request) {
	d.updateTailnet(w, r, false, func(req selectionReq, tc *TailnetConf, peers []string) error {
		if req.Hostname != "" && !validDomain(req.Hostname) {
			return clientError{fmt.Sprintf("invalid hostname %q: lowercase labels of [a-z0-9-], 1-63 chars each", req.Hostname)}
		}
		tc.Hostname = req.Hostname
		return nil
	}, nil)
}

// domainRE matches a lowercase DNS name: labels of 1-63 [a-z0-9-] that
// neither start nor end with a hyphen, dot-separated.
var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func validDomain(s string) bool {
	return len(s) <= 253 && domainRE.MatchString(s)
}

// tldWarning flags friendly domains that collide with public TLD space: a
// single label of ≤4 chars (dev, app, io, ai, sh, me, tv, so, to, co…) is
// very likely a real gTLD. With the alias fall-through, unknown names under
// it still resolve publicly — only names matching a peer's short name shadow
// public domains — so this informs rather than blocks.
func tldWarning(domain string) string {
	if domain == "" || strings.Contains(domain, ".") || len(domain) > 4 {
		return ""
	}
	return fmt.Sprintf("warning: \"%s\" is short enough to be a public TLD (*.%s) — unknown names still resolve publicly, but peer names shadow public ones", domain, domain)
}

// validTailnetName accepts any printable name that is safe as a state-dir
// path component and a URL segment. DNS-safety is not required here: the
// friendly domain and the node hostname are derived via slugify.
func validTailnetName(s string) bool {
	if s == "" || len(s) > 63 || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/\\\x00\n\r\t")
}

// slugify derives a DNS-safe label from a free-form tailnet name: anything
// that isn't [a-z0-9] becomes '-', runs collapse, edges trim, empty → "tailnet".
func slugify(s string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			hyphen = false
		default:
			if !hyphen {
				b.WriteByte('-')
				hyphen = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 { // slug output is pure ASCII, so a byte slice is safe
		out = strings.Trim(out[:63], "-")
	}
	if out == "" {
		return "tailnet"
	}
	return out
}

// handleAddTailnet adds a tailnet at runtime: cidr/tun are optional and
// picked free when omitted (next /24 in 198.18.0.0/15, first free tsm<N>).
// The entry lands in the config (comments elsewhere survive) and starts
// immediately — a fresh tailnet boots into NeedsLogin, so the next step is
// `ts-multinet login <name>` or the web UI's login button.
func (d *Daemon) handleAddTailnet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		CIDR     string `json:"cidr"`
		TUN      string `json:"tun"`
		Domain   string `json:"domain"`
		Hostname string `json:"hostname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "body must be {\"name\": \"...\", \"cidr\"?, \"tun\"?, \"domain\"?}")
		return
	}
	req.Name = strings.TrimSpace(req.Name) // casing is kept — the name is an identity label, not a DNS label
	if !validTailnetName(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid name "+strconv.Quote(req.Name)+": printable, no slashes, 1-63 chars — a DNS-safe domain and node hostname are generated from it")
		return
	}
	if req.Domain != "" && !validDomain(req.Domain) {
		writeErr(w, http.StatusBadRequest, "invalid domain "+strconv.Quote(req.Domain))
		return
	}
	if req.Hostname != "" && !validDomain(req.Hostname) {
		writeErr(w, http.StatusBadRequest, "invalid hostname "+strconv.Quote(req.Hostname)+": lowercase labels of [a-z0-9-], 1-63 chars each")
		return
	}

	tc, err := func() (TailnetConf, error) {
		d.cfgMu.Lock()
		defer d.cfgMu.Unlock()
		cfg, err := loadConfig(d.cfgPath)
		if err != nil {
			return TailnetConf{}, err
		}
		if confByName(cfg, req.Name) != nil {
			return TailnetConf{}, clientError{fmt.Sprintf("tailnet %q already exists", req.Name)}
		}
		cidr := req.CIDR
		if cidr == "" {
			if cidr, err = nextFreeCIDR(cfg); err != nil {
				return TailnetConf{}, clientError{err.Error()}
			}
		} else if err := checkCIDR(cidr, cfg); err != nil {
			return TailnetConf{}, clientError{err.Error()}
		}
		tun := req.TUN
		if tun == "" {
			if tun, err = firstFreeTUN(cfg); err != nil {
				return TailnetConf{}, clientError{err.Error()}
			}
		} else if len(tun) > 15 {
			return TailnetConf{}, clientError{fmt.Sprintf("tun name %q exceeds 15 chars", tun)}
		}
		domain := req.Domain
		if domain == "" {
			domain = slugify(req.Name) // pre-populate the friendly suffix; still editable
		}
		return TailnetConf{Name: req.Name, CIDR: cidr, TUN: tun, Domain: domain, Hostname: req.Hostname}, nil
	}()
	if err != nil {
		var ce clientError
		if errors.As(err, &ce) {
			writeErr(w, http.StatusBadRequest, ce.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	notice, err := d.applyConfigUpdate(func(c *Config) error {
		c.Tailnets = append(c.Tailnets, tc)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := map[string]string{"ok": "tailnet added and started — `login` next", "name": tc.Name, "cidr": tc.CIDR, "tun": tc.TUN}
	if tc.Domain != "" {
		res["domain"] = tc.Domain
	}
	if w := tldWarning(tc.Domain); w != "" {
		res["ok"] += "; " + w
	}
	if notice != "" {
		res["ok"] += "; " + notice
	}
	writeJSON(w, res)
}

// handleRemoveTailnet stops a tailnet and drops it from the config. Node
// state (login identity, selection pins) is kept under state_dir, so a later
// re-add comes back up Running without a new browser login — deleting state
// is a manual rm.
func (d *Daemon) handleRemoveTailnet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := func() (*TailnetConf, error) {
		d.cfgMu.Lock()
		defer d.cfgMu.Unlock()
		cfg, err := loadConfig(d.cfgPath)
		if err != nil {
			return nil, err
		}
		if confByName(cfg, name) == nil {
			return nil, clientError{fmt.Sprintf("no such tailnet %q", name)}
		}
		return nil, nil
	}(); err != nil {
		var ce clientError
		if errors.As(err, &ce) {
			writeErr(w, http.StatusNotFound, ce.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	notice, err := d.applyConfigUpdate(func(c *Config) error {
		c.Tailnets = slices.DeleteFunc(c.Tailnets, func(tc TailnetConf) bool {
			return strings.EqualFold(tc.Name, name)
		})
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok := "tailnet stopped and removed from config (node state kept)"
	if notice != "" {
		ok += "; " + notice
	}
	writeJSON(w, map[string]string{"ok": ok})
}

// syntheticRange is the RFC 2544 benchmark space every per-tailnet range is
// carved from — never seen in real traffic, so a plain route per TUN works.
var syntheticRange = mustCIDR("198.18.0.0/15")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// checkCIDR validates a user-supplied cidr: IPv4, inside the synthetic range,
// and not overlapping any configured tailnet's range.
func checkCIDR(cidr string, cfg *Config) error {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("invalid IPv4 cidr %q", cidr)
	}
	if !syntheticRange.Contains(ipnet.IP) {
		return fmt.Errorf("cidr %s is outside the synthetic range 198.18.0.0/15", cidr)
	}
	for _, tc := range cfg.Tailnets {
		if _, used, err := net.ParseCIDR(tc.CIDR); err == nil && (used.Contains(ipnet.IP) || ipnet.Contains(used.IP)) {
			return fmt.Errorf("cidr %s overlaps tailnet %q (%s)", cidr, tc.Name, tc.CIDR)
		}
	}
	return nil
}

// nextFreeCIDR scans 198.18.0.0/15 for the first /24 no configured tailnet
// uses, in the order the example documents (198.18.1.0/24, .2.0/24, …).
func nextFreeCIDR(cfg *Config) (string, error) {
	probe := &net.IPNet{IP: net.IPv4(198, 18, 1, 0).To4(), Mask: net.CIDRMask(24, 32)}
	for i := 0; i < 512; i++ { // 198.18.1.0 .. 198.19.255.0 minus the broadcast-ish edges
		if err := checkCIDR(probe.String(), cfg); err == nil {
			return probe.String(), nil
		}
		probe.IP[2]++
		if probe.IP[2] == 0 { // wrapped into the next /16
			probe.IP[1]++
		}
	}
	return "", fmt.Errorf("no free /24 left in 198.18.0.0/15")
}

// firstFreeTUN picks the first unused tsm<N> (kernel limit: 15 chars, so N
// stays two digits).
func firstFreeTUN(cfg *Config) (string, error) {
	used := make(map[string]bool, len(cfg.Tailnets))
	for _, tc := range cfg.Tailnets {
		used[tc.TUN] = true
	}
	for n := 0; n < 100; n++ {
		name := fmt.Sprintf("tsm%d", n)
		if !used[name] {
			return name, nil
		}
	}
	return "", fmt.Errorf("no free tsm<N> name")
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
