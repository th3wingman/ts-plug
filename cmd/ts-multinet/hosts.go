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

// hostsEntry is one selected resource rendered into the managed block.
type hostsEntry struct {
	IP      string // synthetic 198.18.x
	Alias   string // friendly alias, e.g. "nucbox.skynet"
	FQDN    string // canonical MagicDNS name
	Tailnet string // config tailnet name (documentation in the line comment)
}

func hostsLine(e hostsEntry) string {
	return fmt.Sprintf("%s %s %s  # ts-multinet (%s)", e.IP, e.Alias, e.FQDN, e.Tailnet)
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

	mode := os.FileMode(0644)
	if err == nil {
		if fi, statErr := os.Stat(path); statErr == nil {
			mode = fi.Mode().Perm()
		}
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), mode)
}
