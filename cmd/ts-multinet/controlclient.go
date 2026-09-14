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
	return json.NewDecoder(resp.Body).Decode(out)
}

// controlGet queries the daemon's unix socket and decodes JSON into out.
func controlGet(sock, path string, out any) error {
	return controlDo(sock, http.MethodGet, path, nil, out)
}

func runStatusClient(sock string) {
	var sts []tailnetStatusJSON
	if err := controlGet(sock, "/status", &sts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%-14s %-22s %-16s %-16s %s\n", "TAILNET", "SUFFIX", "OUR IP", "CIDR", "PEERS")
	for _, s := range sts {
		fmt.Printf("%-14s %-22s %-16s %-16s %d up / %d\n",
			s.Name, s.Suffix, s.AssignedIP, s.CIDR, s.Up, s.Peers)
	}
}

func runPeersClient(sock, filter, ports string) {
	q := url.Values{}
	if filter != "" {
		q.Set("filter", filter)
	}
	if ports != "" {
		q.Set("ports", ports)
	}
	var tps []tailnetPeersJSON
	if err := controlGet(sock, "/peers?"+q.Encode(), &tps); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, tp := range tps {
		hint := ""
		if filter == "" && len(tp.Peers) > 25 {
			hint = "  (tip: filter, e.g. `peers connector`)"
		}
		fmt.Printf("\n== %s (%s) — %d shown, %d up ==%s\n", tp.Name, tp.Suffix, len(tp.Peers), tp.Up, hint)
		fmt.Printf("  %-5s %-34s %-16s %-7s %s\n", "STATE", "NAME", "IP", "OS", "SERVICES")
		for _, p := range tp.Peers {
			state := "down"
			if p.Online {
				state = "UP"
			}
			svc := "—"
			if len(p.Services) > 0 {
				parts := make([]string, len(p.Services))
				for i, port := range p.Services {
					parts[i] = ":" + strconv.Itoa(port)
				}
				svc = strings.Join(parts, " ")
			}
			fmt.Printf("  %-5s %-34s %-16s %-7s %s\n", state, truncate(p.Name, 34), p.IP, p.OS, svc)
		}
	}
}

func runCheckClient(sock, target string) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		host, portStr = target, "80"
	}
	q := url.Values{}
	q.Set("host", host)
	q.Set("port", portStr)
	var res checkJSON
	if err := controlGet(sock, "/check?"+q.Encode(), &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("host:      %s\n", res.Host)
	switch res.Result {
	case "no_tailnet":
		fmt.Printf("result:    no configured tailnet matches that suffix\n")
		return
	case "resolve_failed":
		if res.Tailnet != "" {
			fmt.Printf("tailnet:   %s (%s)\n", res.Tailnet, res.Suffix)
			fmt.Printf("resolve:   FAILED — not a known peer on %s\n", res.Tailnet)
			fmt.Printf("           (run `peers %s` to see what exists)\n", res.Tailnet)
		} else {
			fmt.Printf("resolve:   FAILED — no peer by that name on any tailnet\n")
			fmt.Printf("           (run `peers` to list, or try host.<tailnet>)\n")
		}
		return
	}
	fmt.Printf("tailnet:   %s (%s)\n", res.Tailnet, res.Suffix)
	fmt.Printf("resolve:   %s -> %s\n", res.Host, res.ResolvedIP)
	switch res.Result {
	case "unreachable":
		fmt.Printf("result:    UNREACHABLE (%dms) — %s\n", res.LatencyMS, res.Detail)
	case "open":
		if res.Banner != "" {
			fmt.Printf("result:    OPEN (%dms) — banner: %s\n", res.LatencyMS, res.Banner)
		} else {
			fmt.Printf("result:    OPEN (%dms) — connected, no banner (server speaks first? try HTTP)\n", res.LatencyMS)
		}
	}
}

// controlPost posts JSON to the daemon's unix socket and decodes the response.
func controlPost(sock, path string, body any, out any) error {
	return controlDo(sock, http.MethodPost, path, body, out)
}

// runAddClient adds a tailnet live. cidr/tun are optional and come as a pair;
// the server auto-picks the next free synthetic /24 and tsm<N> when omitted.
func runAddClient(sock, name, cidr, tun string) {
	body := map[string]string{"name": name}
	if cidr != "" {
		body["cidr"], body["tun"] = cidr, tun
	}
	var res struct {
		OK     string `json:"ok"`
		Error  string `json:"error"`
		Name   string `json:"name"`
		CIDR   string `json:"cidr"`
		TUN    string `json:"tun"`
		Domain string `json:"domain"`
	}
	if err := controlPost(sock, "/tailnet", body, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, "add: "+res.Error)
		os.Exit(1)
	}
	fmt.Printf("added %s — cidr %s, tun %s\n", res.Name, res.CIDR, res.TUN)
	if res.Domain != "" {
		fmt.Printf("domain %s\n", res.Domain)
	}
	fmt.Printf("next: sudo ts-multinet login %s\n", res.Name)
}

// runRemoveClient stops a tailnet and drops it from the config. Node state
// is kept, so the printed re-add hint needs no new browser login.
func runRemoveClient(sock, name string) {
	var res struct {
		OK    string `json:"ok"`
		Error string `json:"error"`
	}
	if err := controlDo(sock, http.MethodDelete, "/tailnet/"+name, nil, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, "remove: "+res.Error)
		os.Exit(1)
	}
	fmt.Println(res.OK)
	fmt.Printf("re-add anytime: sudo ts-multinet add %s — logs back in without a browser\n", name)
}

func runLoginClient(sock, tailnet string) {
	if tailnet == "" {
		// No argument: report login state for every tailnet.
		var sts []tailnetStatusJSON
		if err := controlGet(sock, "/status", &sts); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
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
	if err := controlPost(sock, "/login", map[string]string{"tailnet": tailnet}, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
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

func runReloadClient(sock string) {
	var res map[string]string
	if err := controlPost(sock, "/reload", map[string]string{}, &res); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(res["ok"])
}

// mutate posts one /tailnet/{name}/... mutation and returns the daemon's
// error text, if any. Both the success and error replies are flat objects
// ("ok"/"error"), so one decode covers both.
func mutate(sock, path string, body any) error {
	var res struct {
		OK    string `json:"ok"`
		Error string `json:"error"`
	}
	if err := controlPost(sock, path, body, &res); err != nil {
		return err
	}
	if res.Error != "" {
		return errors.New(res.Error)
	}
	return nil
}

// runSelectForgetClient drives select/forget: one POST per peer, stopping at
// the first failure so a bad name doesn't get lost in the noise.
func runSelectForgetClient(sock, tailnet, verb string, peers []string) {
	if tailnet == "" || len(peers) == 0 {
		fmt.Fprintf(os.Stderr, "usage: ts-multinet %s <tailnet> <peer> [peer...]\n", verb)
		os.Exit(1)
	}
	for _, p := range peers {
		if err := mutate(sock, "/tailnet/"+tailnet+"/"+verb, map[string]string{"peer": p}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("%s %s\n", verb, p)
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

func runAllowAllClient(sock, tailnet, arg string) {
	if tailnet == "" {
		fmt.Fprintln(os.Stderr, "usage: ts-multinet allow-all <tailnet> [on|off]")
		os.Exit(1)
	}
	on, err := parseOnOff(arg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "allow-all: "+err.Error())
		os.Exit(1)
	}
	if err := mutate(sock, "/tailnet/"+tailnet+"/allow-all", map[string]bool{"on": on}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if on {
		fmt.Printf("allow-all %s on — every non-Mullvad peer selected\n", tailnet)
	} else {
		fmt.Printf("allow-all %s off\n", tailnet)
	}
}

// runDomainClient sets a tailnet's friendly DNS suffix ("-" clears the
// override); with no argument it prints the effective domain.
func runDomainClient(sock, tailnet, arg string) {
	if tailnet == "" {
		fmt.Fprintln(os.Stderr, "usage: ts-multinet domain <tailnet> [name|-]")
		os.Exit(1)
	}
	if arg == "" {
		var cfg Config
		if err := controlGet(sock, "/config", &cfg); err != nil {
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
		fmt.Printf("%s: %s\n", tailnet, tc.domainName())
		return
	}
	if arg == "-" {
		arg = ""
	}
	if err := mutate(sock, "/tailnet/"+tailnet+"/domain", map[string]string{"domain": arg}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if arg == "" {
		fmt.Printf("domain cleared for %s (back to the tailnet name)\n", tailnet)
	} else {
		fmt.Printf("domain set: %s.%s\n", "<peer>", arg)
	}
}

// runConfigClient pretty-prints the daemon's effective config as-is.
func runConfigClient(sock string) {
	var raw json.RawMessage
	if err := controlGet(sock, "/config", &raw); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
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
