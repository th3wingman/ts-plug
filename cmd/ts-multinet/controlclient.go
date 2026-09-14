// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// controlGet queries the daemon's unix socket and decodes JSON into out.
func controlGet(sock, path string, out any) error {
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := c.Get("http://unix" + path)
	if err != nil {
		return fmt.Errorf("no daemon at %s — is it running? (%w)", sock, err)
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
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
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := c.Post("http://unix"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("no daemon at %s — is it running? (%w)", sock, err)
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
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
