// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// controlDo sends a JSON request with any method to the daemon's unix socket
// and decodes the response; the verbs below are thin wrappers over it.
func controlDo(sock, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequest(method, "http://unix"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("no daemon at %s — is it running? (%w)", sock, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// the daemon answers {"error": "..."} on 4xx/5xx, but a plain-text
		// body (stale daemon, wrong route) must not leak as a json decode error
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("daemon: %s", e.Error)
		}
		return fmt.Errorf("daemon: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.Unmarshal(b, out)
}

// controlGet queries the daemon's unix socket and decodes JSON into out.
func controlGet(sock, path string, out any) error {
	return controlDo(sock, http.MethodGet, path, nil, out)
}

// cliOpts carries the control socket and the output flags to every runner;
// --json switches stdout to machine-readable output (errors stay on stderr
// as text, exit codes unchanged), --details extends both human and json forms.
type cliOpts struct {
	sock    string
	json    bool
	details bool
}

// marshalPretty is the one JSON encoder every --json path uses, so tests can
// pin the shape without a socket.
func marshalPretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func emitJSON(v any) {
	fmt.Println(marshalPretty(v))
}

// mutateResult is the flat reply every /tailnet mutation returns.
type mutateResult struct {
	OK    string `json:"ok"`
	Error string `json:"error"`
}

func runStatusClient(c cliOpts) {
	var sts []tailnetStatusJSON
	if err := controlGet(c.sock, "/status", &sts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(sts) // the server reply already carries every field; --details adds nothing here
		return
	}
	if c.details {
		// the effective domain comes from the config (status does not carry it);
		// the service breakdown is best-effort — an unavailable list must not
		// fail status itself.
		var cfg Config
		if err := controlGet(c.sock, "/config", &cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var svcs []tailnetServicesJSON
		_ = controlGet(c.sock, "/services", &svcs)
		fmt.Print(formatStatusDetails(sts, cfg, svcs))
		return
	}
	fmt.Print(formatStatusTable(sts))
}

func formatStatusTable(sts []tailnetStatusJSON) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-14s %-22s %-16s %-16s %s\n", "TAILNET", "SUFFIX", "OUR IP", "CIDR", "PEERS")
	for _, s := range sts {
		fmt.Fprintf(&b, "%-14s %-22s %-16s %-16s %d up / %d\n",
			s.Name, s.Suffix, s.AssignedIP, s.CIDR, s.Up, s.Peers)
	}
	return b.String()
}

func formatStatusDetails(sts []tailnetStatusJSON, cfg Config, svcs []tailnetServicesJSON) string {
	var b strings.Builder
	for _, s := range sts {
		domain := ""
		if tc := confByName(&cfg, s.Name); tc != nil {
			domain = tc.domainName()
		}
		fmt.Fprintf(&b, "%s — %s\n", s.Name, orDefault(s.State, "?"))
		fmt.Fprintf(&b, "  suffix    %s\n", orDefault(s.Suffix, "(detecting)"))
		fmt.Fprintf(&b, "  domain    %s\n", orDefault(domain, s.Name))
		fmt.Fprintf(&b, "  hostname  %s\n", s.Hostname)
		fmt.Fprintf(&b, "  our ip    %s\n", orDefault(s.AssignedIP, "—"))
		fmt.Fprintf(&b, "  cidr      %s\n", s.CIDR)
		fmt.Fprintf(&b, "  peers     %d up / %d total, %d selected\n", s.Up, s.Peers, s.Selected)
		if tp := servicesFor(svcs, s.Name); tp != nil {
			fmt.Fprintf(&b, "  services  %d advertised / %d selected\n", len(tp.Services), selectedServices(tp.Services))
		}
		if s.LoginURL != "" {
			fmt.Fprintf(&b, "  login     %s\n", s.LoginURL)
		}
	}
	return b.String()
}

// runServicesClient lists the advertised VIP services the daemon can see on
// each tailnet (optionally one), with their selection state.
func runServicesClient(c cliOpts, tailnet string) {
	var tss []tailnetServicesJSON
	if err := controlGet(c.sock, "/services", &tss); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if tailnet != "" {
		var filtered []tailnetServicesJSON
		for _, ts := range tss {
			if strings.EqualFold(ts.Name, tailnet) {
				filtered = append(filtered, ts)
			}
		}
		if len(filtered) == 0 {
			fmt.Fprintf(os.Stderr, "%s: no such tailnet\n", tailnet)
			os.Exit(1)
		}
		tss = filtered
	}
	if c.json {
		emitJSON(tss)
		return
	}
	if c.details {
		fmt.Print(formatServicesDetails(tss))
		return
	}
	fmt.Print(formatServicesTable(tss))
}

// servicesFor finds one tailnet's service entry, or nil when the daemon
// reported none for it.
func servicesFor(tss []tailnetServicesJSON, name string) *tailnetServicesJSON {
	for i := range tss {
		if tss[i].Name == name {
			return &tss[i]
		}
	}
	return nil
}

func selectedServices(ss []serviceJSON) int {
	n := 0
	for _, s := range ss {
		if s.Selected {
			n++
		}
	}
	return n
}

func formatServicesTable(tss []tailnetServicesJSON) string {
	var b strings.Builder
	total := 0
	for _, ts := range tss {
		total += len(ts.Services)
		fmt.Fprintf(&b, "\n== %s (%s) — %d service%s ==\n", ts.Name, ts.Suffix, len(ts.Services), plural(len(ts.Services)))
		if len(ts.Services) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %-5s %-22s %-20s %-16s %s\n", "SEL", "NAME", "DISPLAY", "VIP", "PORTS")
		for _, s := range ts.Services {
			fmt.Fprintf(&b, "  %-5s %-22s %-20s %-16s %s\n", selMark(s.Selected), truncate(s.Name, 22), truncate(s.DisplayName, 20), orDefault(firstVIP(s), "—"), orDefault(strings.Join(s.Ports, " "), "—"))
		}
	}
	if total == 0 {
		b.WriteString("\nno advertised services visible — service visibility is ACL-gated on the tailnet\n")
	}
	return b.String()
}

func formatServicesDetails(tss []tailnetServicesJSON) string {
	var b strings.Builder
	for _, ts := range tss {
		fmt.Fprintf(&b, "\n== %s (%s) ==\n", ts.Name, ts.Suffix)
		if len(ts.Services) == 0 {
			b.WriteString("  (none visible — service visibility is ACL-gated)\n")
			continue
		}
		for _, s := range ts.Services {
			fmt.Fprintf(&b, "  %s — %s\n", s.Name, s.DisplayName)
			fmt.Fprintf(&b, "    selected  %v\n", s.Selected)
			fmt.Fprintf(&b, "    vips      %s\n", orDefault(strings.Join(s.VIPs, ", "), "—"))
			fmt.Fprintf(&b, "    ports     %s\n", orDefault(strings.Join(s.Ports, ", "), "—"))
		}
	}
	return b.String()
}

func firstVIP(s serviceJSON) string {
	if len(s.VIPs) == 0 {
		return ""
	}
	return s.VIPs[0]
}

func selMark(sel bool) string {
	if sel {
		return "[x]"
	}
	return "[ ]"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func runPeersClient(c cliOpts, filter, ports string) {
	q := url.Values{}
	if filter != "" {
		q.Set("filter", filter)
	}
	if ports != "" {
		q.Set("ports", ports)
	}
	var tps []tailnetPeersJSON
	if err := controlGet(c.sock, "/peers?"+q.Encode(), &tps); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(tps)
		return
	}
	if c.details {
		fmt.Print(formatPeersDetails(tps, filter))
		return
	}
	fmt.Print(formatPeersTable(tps, filter))
}

func formatPeersTable(tps []tailnetPeersJSON, filter string) string {
	var b strings.Builder
	for _, tp := range tps {
		hint := ""
		if filter == "" && len(tp.Peers) > 25 {
			hint = "  (tip: filter, e.g. `peers connector`)"
		}
		fmt.Fprintf(&b, "\n== %s (%s) — %d shown, %d up ==%s\n", tp.Name, tp.Suffix, len(tp.Peers), tp.Up, hint)
		fmt.Fprintf(&b, "  %-5s %-34s %-16s %-7s %s\n", "STATE", "NAME", "IP", "OS", "SERVICES")
		for _, p := range tp.Peers {
			fmt.Fprintf(&b, "  %-5s %-34s %-16s %-7s %s\n", stateOf(p), truncate(p.Name, 34), p.IP, p.OS, servicesOf(p))
		}
	}
	return b.String()
}

func formatPeersDetails(tps []tailnetPeersJSON, _ string) string { // filter applied server-side
	var b strings.Builder
	for _, tp := range tps {
		fmt.Fprintf(&b, "\n== %s (%s) — %d shown, %d up ==\n", tp.Name, tp.Suffix, len(tp.Peers), tp.Up)
		fmt.Fprintf(&b, "  %-5s %-28s %-40s %-16s %-7s %s\n", "STATE", "NAME", "FQDN", "IP", "OS", "SERVICES")
		for _, p := range tp.Peers {
			fmt.Fprintf(&b, "  %-5s %-28s %-40s %-16s %-7s %s\n", stateOf(p), truncate(p.Name, 28), truncate(orDefault(p.FQDN, "—"), 40), p.IP, p.OS, servicesOf(p))
		}
	}
	return b.String()
}

func stateOf(p peerJSON) string {
	if p.Online {
		return "UP"
	}
	return "down"
}

func servicesOf(p peerJSON) string {
	if len(p.Services) == 0 {
		return "—"
	}
	parts := make([]string, len(p.Services))
	for i, port := range p.Services {
		parts[i] = ":" + strconv.Itoa(port)
	}
	return strings.Join(parts, " ")
}

func runCheckClient(c cliOpts, target string) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		host, portStr = target, "80"
	}
	q := url.Values{}
	q.Set("host", host)
	q.Set("port", portStr)
	var res checkJSON
	if err := controlGet(c.sock, "/check?"+q.Encode(), &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(res)
		return
	}
	fmt.Print(formatCheck(res))
	if c.details {
		fmt.Print(formatCheckDetails(res))
	}
}

func formatCheck(res checkJSON) string {
	var b strings.Builder
	fmt.Fprintf(&b, "host:      %s\n", res.Host)
	switch res.Result {
	case "no_tailnet":
		fmt.Fprintf(&b, "result:    no configured tailnet matches that suffix\n")
	case "resolve_failed":
		if res.Tailnet != "" {
			fmt.Fprintf(&b, "tailnet:   %s (%s)\n", res.Tailnet, res.Suffix)
			fmt.Fprintf(&b, "resolve:   FAILED — not a known peer on %s\n", res.Tailnet)
			fmt.Fprintf(&b, "           (run `peers %s` to see what exists)\n", res.Tailnet)
		} else {
			fmt.Fprintf(&b, "resolve:   FAILED — no peer by that name on any tailnet\n")
			fmt.Fprintf(&b, "           (run `peers` to list, or try host.<tailnet>)\n")
		}
	default:
		fmt.Fprintf(&b, "tailnet:   %s (%s)\n", res.Tailnet, res.Suffix)
		fmt.Fprintf(&b, "resolve:   %s -> %s\n", res.Host, res.ResolvedIP)
		switch res.Result {
		case "unreachable":
			fmt.Fprintf(&b, "result:    UNREACHABLE (%dms) — %s\n", res.LatencyMS, res.Detail)
		case "open":
			if res.Banner != "" {
				fmt.Fprintf(&b, "result:    OPEN (%dms) — banner: %s\n", res.LatencyMS, res.Banner)
			} else {
				fmt.Fprintf(&b, "result:    OPEN (%dms) — connected, no banner (server speaks first? try HTTP)\n", res.LatencyMS)
			}
		}
	}
	return b.String()
}

// formatCheckDetails appends the raw fields for --details.
func formatCheckDetails(res checkJSON) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nresolved:  %s\n", orDefault(res.ResolvedIP, "—"))
	fmt.Fprintf(&b, "tailnet:   %s\n", orDefault(res.Tailnet, "—"))
	fmt.Fprintf(&b, "suffix:    %s\n", orDefault(res.Suffix, "—"))
	fmt.Fprintf(&b, "latency:   %dms\n", res.LatencyMS)
	fmt.Fprintf(&b, "banner:    %s\n", orDefault(res.Banner, "—"))
	if res.Detail != "" {
		fmt.Fprintf(&b, "detail:    %s\n", res.Detail)
	}
	return b.String()
}

// controlPost posts JSON to the daemon's unix socket and decodes the response.
func controlPost(sock, path string, body any, out any) error {
	return controlDo(sock, http.MethodPost, path, body, out)
}

// runAddClient adds a tailnet live. cidr/tun are optional and come as a pair;
// the server auto-picks the next free synthetic /24 and tsm<N> when omitted.
func runAddClient(c cliOpts, name, cidr, tun string) {
	body := map[string]string{"name": name}
	if cidr != "" {
		body["cidr"], body["tun"] = cidr, tun
	}
	var res struct {
		OK         string `json:"ok"`
		Error      string `json:"error"`
		Name       string `json:"name"`
		CIDR       string `json:"cidr"`
		TUN        string `json:"tun"`
		Domain     string `json:"domain"`
		AuthKeySet string `json:"auth_key_set,omitempty"`
	}
	if err := controlPost(c.sock, "/tailnet", body, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, "add: "+res.Error)
		os.Exit(1)
	}
	if c.json {
		emitJSON(res)
		return
	}
	fmt.Printf("added %s — cidr %s, tun %s\n", res.Name, res.CIDR, res.TUN)
	if res.Domain != "" {
		fmt.Printf("domain %s\n", res.Domain)
	}
	if res.AuthKeySet == "true" {
		fmt.Println("auth key stored — enrolling without a browser (tagged per the key)")
		return
	}
	fmt.Printf("next: sudo ts-multinet login %s\n", res.Name)
}

// runRemoveClient stops a tailnet and drops it from the config. Node state
// is kept, so the printed re-add hint needs no new browser login.
func runRemoveClient(c cliOpts, name string) {
	var res mutateResult
	if err := controlDo(c.sock, http.MethodDelete, "/tailnet/"+url.PathEscape(name), nil, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, "remove: "+res.Error)
		os.Exit(1)
	}
	if c.json {
		emitJSON(res)
		return
	}
	fmt.Println(res.OK)
	fmt.Printf("re-add anytime: sudo ts-multinet add %s — logs back in without a browser\n", name)
}

func runLoginClient(c cliOpts, tailnet, authkey string) {
	if authkey != "" {
		// Key enrollment: no browser — the node joins tagged per the key.
		if tailnet == "" {
			fmt.Fprintln(os.Stderr, "usage: ts-multinet login <tailnet> <authkey>")
			os.Exit(1)
		}
		r, err := mutate(c.sock, "/tailnet/"+url.PathEscape(tailnet)+"/authkey", map[string]string{"auth_key": authkey})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if c.json {
			emitJSON(r)
			return
		}
		fmt.Println(r.OK)
		return
	}
	if tailnet == "" {
		// No argument: report login state for every tailnet.
		var sts []tailnetStatusJSON
		if err := controlGet(c.sock, "/status", &sts); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if c.json {
			emitJSON(sts)
			return
		}
		for _, s := range sts {
			switch s.State {
			case "Running":
				fmt.Printf("%-14s running\n", s.Name)
			case "NeedsLogin":
				fmt.Printf("%-14s needs login: ts-multinet login %s\n", s.Name, s.Name)
			default:
				fmt.Printf("%-14s %s\n", s.Name, s.State)
			}
		}
		return
	}
	var res loginResult
	if err := controlPost(c.sock, "/login", map[string]string{"tailnet": tailnet}, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(res)
		return
	}
	if res.Error != "" {
		fmt.Fprintf(os.Stderr, "%s: %s (state: %s)\n", res.Tailnet, res.Error, res.State)
		os.Exit(1)
	}
	if res.LoginURL != "" {
		fmt.Printf("open in a browser to join %s:\n\n  %s\n\n", res.Tailnet, res.LoginURL)
		fmt.Println("the tailnet connects once you authenticate; state persists across restarts")
		return
	}
	fmt.Printf("%s: %s — no login needed\n", res.Tailnet, res.State)
}

func runReloadClient(c cliOpts) {
	var res map[string]string
	if err := controlPost(c.sock, "/reload", map[string]string{}, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(res)
		return
	}
	fmt.Println(res["ok"])
}

// runRestartClient stops and restarts one tailnet in place (node state, and
// login, kept) — the manual recovery for a node a network outage left dark.
func runRestartClient(c cliOpts, tailnet string) {
	if tailnet == "" {
		fmt.Fprintln(os.Stderr, "usage: ts-multinet restart <tailnet>")
		os.Exit(1)
	}
	r, err := mutate(c.sock, "/tailnet/"+url.PathEscape(tailnet)+"/restart", map[string]string{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(r)
		return
	}
	fmt.Println(r.OK)
}

// runEnabledClient flips a tailnet's enabled flag: disable stops the node
// (state and login kept), enable starts it again — no unprovisioning.
func runEnabledClient(c cliOpts, tailnet string, on bool) {
	r, err := mutate(c.sock, "/tailnet/"+url.PathEscape(tailnet)+"/enabled", map[string]bool{"on": on})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(r)
		return
	}
	fmt.Println(r.OK)
}

// runHostnameClient sets the node name reported inside the tailnet (an
// empty-string / "-" argument clears the override); with no argument it
// prints the effective hostname.
func runHostnameClient(c cliOpts, tailnet, arg string) {
	if tailnet == "" {
		fmt.Fprintln(os.Stderr, "usage: ts-multinet hostname <tailnet> [name|-]")
		os.Exit(1)
	}
	if arg == "" || arg == "-" {
		var cfg Config
		if err := controlGet(c.sock, "/config", &cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for i := range cfg.Tailnets {
			if strings.EqualFold(cfg.Tailnets[i].Name, tailnet) {
				if c.json {
					emitJSON(map[string]string{"tailnet": tailnet, "hostname": cfg.Tailnets[i].nodeHostname()})
					return
				}
				fmt.Println(cfg.Tailnets[i].nodeHostname())
				if arg == "" {
					return
				}
				break
			}
		}
	}
	r, err := mutate(c.sock, "/tailnet/"+url.PathEscape(tailnet)+"/hostname", map[string]string{"hostname": arg})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(r)
		return
	}
	fmt.Println("hostname set — the tailnet restarts to report it (no re-login)")
}

// mutate posts one /tailnet/{name}/... mutation and returns the daemon's
// reply; a non-nil error covers transport failures and server-side errors
// (the flat "error" field) alike.
func mutate(sock, path string, body any) (mutateResult, error) {
	var res mutateResult
	if err := controlPost(sock, path, body, &res); err != nil {
		return res, err
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

// runSelectForgetClient drives select/forget: one POST per peer, stopping at
// the first failure so a bad name doesn't get lost in the noise.
func runSelectForgetClient(c cliOpts, tailnet, verb string, peers []string) {
	if tailnet == "" || len(peers) == 0 {
		fmt.Fprintf(os.Stderr, "usage: ts-multinet %s <tailnet> <peer> [peer...]\n", verb)
		os.Exit(1)
	}
	for _, p := range peers {
		r, err := mutate(c.sock, "/tailnet/"+url.PathEscape(tailnet)+"/"+verb, map[string]string{"peer": p})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if c.json {
			emitJSON(r) // one object per peer (JSONL) so multi-peer output streams
			continue
		}
		fmt.Printf("%s %s\n", verb, p)
	}
}

// runLockClient drives lock/unlock: one POST per name, stopping at the
// first failure like select/forget. Locking pins an essential (survives
// clear-all, blocks disabling the tailnet); unlocking releases the pin but
// keeps the selection — forget is the separate step.
func runLockClient(c cliOpts, tailnet string, lock bool, names []string) {
	if tailnet == "" || len(names) == 0 {
		fmt.Fprintf(os.Stderr, "usage: ts-multinet %s <tailnet> <peer|svc:label> [name...]\n", map[bool]string{true: "lock", false: "unlock"}[lock])
		os.Exit(1)
	}
	for _, p := range names {
		r, err := mutate(c.sock, "/tailnet/"+url.PathEscape(tailnet)+"/lock", map[string]any{"peer": p, "on": lock})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if c.json {
			emitJSON(r) // JSONL, same as multi-peer select/forget
			continue
		}
		fmt.Printf("%s %s\n", map[bool]string{true: "lock", false: "unlock"}[lock], p)
	}
}

// parseOnOff maps an optional on/off argument; absent means on.
func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "", "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, fmt.Errorf("expected on or off, got %q", s)
}

func runAllowAllClient(c cliOpts, tailnet, arg string) {
	if tailnet == "" {
		fmt.Fprintln(os.Stderr, "usage: ts-multinet allow-all <tailnet> [on|off]")
		os.Exit(1)
	}
	on, err := parseOnOff(arg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "allow-all: "+err.Error())
		os.Exit(1)
	}
	r, err := mutate(c.sock, "/tailnet/"+url.PathEscape(tailnet)+"/allow-all", map[string]bool{"on": on})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(r)
		return
	}
	if on {
		fmt.Printf("allow-all %s on — every non-Mullvad peer selected\n", tailnet)
	} else {
		fmt.Printf("allow-all %s off\n", tailnet)
	}
}

// runDomainClient sets a tailnet's friendly DNS suffix ("-" clears the
// override); with no argument it prints the effective domain.
func runDomainClient(c cliOpts, tailnet, arg string) {
	if tailnet == "" {
		fmt.Fprintln(os.Stderr, "usage: ts-multinet domain <tailnet> [name|-]")
		os.Exit(1)
	}
	if arg == "" {
		var cfg Config
		if err := controlGet(c.sock, "/config", &cfg); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		tc := confByName(&cfg, tailnet)
		if tc == nil {
			var known []string
			for _, t := range cfg.Tailnets {
				known = append(known, t.Name)
			}
			fmt.Fprintf(os.Stderr, "%s: no such tailnet (known: %s)\n", tailnet, strings.Join(known, ", "))
			os.Exit(1)
		}
		if c.json {
			emitJSON(map[string]string{"tailnet": tailnet, "domain": tc.domainName()})
			return
		}
		fmt.Printf("%s: %s\n", tailnet, tc.domainName())
		return
	}
	if arg == "-" {
		arg = ""
	}
	r, err := mutate(c.sock, "/tailnet/"+url.PathEscape(tailnet)+"/domain", map[string]string{"domain": arg})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if c.json {
		emitJSON(r)
		return
	}
	if arg == "" {
		fmt.Printf("domain cleared for %s (back to the tailnet name)\n", tailnet)
	} else {
		fmt.Printf("domain set: %s.%s\n", "<peer>", arg)
	}
}

// runConfigClient prints the daemon's effective config: pretty by default,
// compact one-line with --json (for piping into jq etc.).
func runConfigClient(c cliOpts) {
	var raw json.RawMessage
	if err := controlGet(c.sock, "/config", &raw); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var buf bytes.Buffer
	if c.json {
		if err := json.Compact(&buf, raw); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	} else if err := json.Indent(&buf, raw, "", "  "); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(buf.String())
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
