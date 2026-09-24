// Copyright (c) Tailscale Inc & Authors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"fmt"
	"os"
	"strings"
)

// The /etc/hosts managed block: everything between the markers is rewritten by
// the daemon on every selection apply; everything outside is preserved
// byte-for-byte.
const (
	hostsBegin = "# ts-multinet begin"
	hostsEnd   = "# ts-multinet end"
)

// hostsEntry is one selected resource rendered into the managed block. Alias
// is built as the bare hostname (or service label) and qualified by
// qualifyHostsAliases when needed. Native includes the canonical MagicDNS
// name on the line: off by default, so hostname completion sees exactly one
// deterministic spelling.
type hostsEntry struct {
	IP      string // synthetic 198.18.x
	Alias   string // written name: bare by default; .<domain> on collision or domain_hosts
	FQDN    string // canonical MagicDNS name
	Tailnet string // config tailnet name (documentation in the line comment)
	Native  bool   // also list FQDN (opt-in per tailnet, native_dns)

	// Domain and ForceDomain are assembly inputs for qualifyHostsAliases;
	// they are never rendered.
	Domain      string // the tailnet's friendly domain
	ForceDomain bool   // domain_hosts: always qualify the alias
}

func hostsLine(e hostsEntry) string {
	if e.Native && e.FQDN != "" && e.FQDN != e.Alias {
		return fmt.Sprintf("%s %s %s  # ts-multinet (%s)", e.IP, e.Alias, e.FQDN, e.Tailnet)
	}
	return fmt.Sprintf("%s %s  # ts-multinet (%s)", e.IP, e.Alias, e.Tailnet)
}

// qualifyHostsAliases picks the name each entry writes: the bare hostname by
// default — like any other /etc/hosts line, so shell completion and lookups
// work with the plain name — qualified with .<domain> only when two tailnets
// claim the same bare name (/etc/hosts resolves duplicates by file order,
// which would silently pick one) or when the tailnet opts into domain_hosts.
// A still-duplicated alias (two tailnets sharing one custom domain) falls
// back to the full MagicDNS name: a name must never appear twice with
// different IPs.
func qualifyHostsAliases(entries []hostsEntry) {
	claims := map[string]map[string]bool{} // bare name -> tailnets writing it bare
	for _, e := range entries {
		if e.ForceDomain {
			continue // qualified from the start; never claims the bare name
		}
		if claims[e.Alias] == nil {
			claims[e.Alias] = map[string]bool{}
		}
		claims[e.Alias][e.Tailnet] = true
	}
	for i := range entries {
		e := &entries[i]
		if e.ForceDomain || len(claims[e.Alias]) > 1 {
			e.Alias += "." + e.Domain
		}
	}
	seen := map[string]string{}  // alias -> first IP seen
	collide := map[string]bool{} // aliases claimed with more than one IP
	for _, e := range entries {
		if ip, dup := seen[e.Alias]; dup && ip != e.IP {
			collide[e.Alias] = true
		} else if !dup {
			seen[e.Alias] = e.IP
		}
	}
	for i := range entries {
		if collide[entries[i].Alias] {
			entries[i].Alias = entries[i].FQDN // every claimer, not just the later ones
		}
	}
}

// writeHostsBlock replaces (or appends) the managed block in path with the
// given entries, in order. The file's existing content outside the block —
// including its permissions — is preserved. An empty entry set leaves the
// markers in place with nothing between them.
func writeHostsBlock(path string, entries []hostsEntry) error {
	content, err := os.ReadFile(path)
	var lines []string
	if err == nil {
		lines = strings.Split(string(content), "\n")
	} else if !os.IsNotExist(err) {
		return err
	}

	block := make([]string, 0, len(entries)+2)
	block = append(block, hostsBegin)
	for _, e := range entries {
		block = append(block, hostsLine(e))
	}
	block = append(block, hostsEnd)

	var out []string
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case hostsBegin:
			begin = i
		case hostsEnd:
			end = i
		}
	}
	switch {
	case begin >= 0 && end >= begin:
		out = append(out, lines[:begin]...)
		out = append(out, block...)
		out = append(out, lines[end+1:]...)
	default:
		out = append(out, lines...)
		// Trim trailing empties so the appended block sits after one blank line.
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, "")
		out = append(out, block...)
	}
	// Normalize: exactly one trailing newline, so rewrites are byte-stable.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	out = append(out, "")

	// Netmap-driven re-applies can land with nothing changed (a peer flapped,
	// a rule matched the same set): byte-stable output means a no-op write is
	// detectable, and skipping it keeps /etc/hosts quiet for watchers.
	if err == nil && strings.Join(out, "\n") == string(content) {
		return nil
	}

	mode := os.FileMode(0644)
	if err == nil {
		if fi, statErr := os.Stat(path); statErr == nil {
			mode = fi.Mode().Perm()
		}
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), mode)
}
