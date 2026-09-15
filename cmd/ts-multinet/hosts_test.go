// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteHostsBlockReplacesManagedBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	original := "127.0.0.1\tlocalhost\n192.168.1.10\tprinter\n# ts-multinet begin\nstale entry\n# ts-multinet end\n1.2.3.4\tlast\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	entries := []hostsEntry{
		{IP: "198.18.1.5", Alias: "nucbox.skynet", FQDN: "nucbox.tail523555.ts.net", Tailnet: "skynet"},
		{IP: "198.18.2.7", Alias: "zombie.msinfra", FQDN: "zombie.tail84a2fd.ts.net", Tailnet: "msinfra"},
	}
	if err := writeHostsBlock(path, entries); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)

	// Outside the block: preserved byte-for-byte, in order.
	for _, want := range []string{
		"127.0.0.1\tlocalhost\n",
		"192.168.1.10\tprinter\n",
		"1.2.3.4\tlast\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("outside content lost: %q missing from:\n%s", want, got)
		}
	}
	if strings.Contains(got, "stale entry") {
		t.Error("stale managed entry survived the rewrite")
	}
	// The managed block: markers plus one line per entry, in order.
	if i, j := strings.Index(got, hostsBegin), strings.Index(got, hostsEnd); i < 0 || j < i {
		t.Fatalf("markers missing or out of order:\n%s", got)
	}
	for _, e := range entries {
		if !strings.Contains(got, hostsLine(e)) {
			t.Errorf("entry line missing: %q", hostsLine(e))
		}
	}

	// Idempotent: rewriting the same entries changes nothing.
	first := got
	if err := writeHostsBlock(path, entries); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if string(again) != first {
		t.Errorf("rewrite is not idempotent:\nfirst:\n%s\nagain:\n%s", first, again)
	}
}

func TestWriteHostsBlockEmptySelectionKeepsMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := writeHostsBlock(path, []hostsEntry{{IP: "198.18.1.5", Alias: "a.skynet", FQDN: "a.tail.ts.net", Tailnet: "skynet"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeHostsBlock(path, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.Contains(got, hostsBegin) || !strings.Contains(got, hostsEnd) {
		t.Errorf("markers must survive an empty selection:\n%s", got)
	}
	if strings.Contains(got, "198.18.1.5") {
		t.Errorf("deselected entry survived:\n%s", got)
	}
}

func TestWriteHostsBlockAppendsWhenNoMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	original := "127.0.0.1\tlocalhost\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeHostsBlock(path, []hostsEntry{{IP: "198.18.1.5", Alias: "a.skynet", FQDN: "a.tail.ts.net", Tailnet: "skynet"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.HasPrefix(got, original) {
		t.Errorf("existing content must lead:\n%s", got)
	}
	if !strings.Contains(got, hostsLine(hostsEntry{IP: "198.18.1.5", Alias: "a.skynet", FQDN: "a.tail.ts.net", Tailnet: "skynet"})) {
		t.Errorf("appended entry missing:\n%s", got)
	}
}

// The native MagicDNS name is opt-in per tailnet (native_dns): off keeps
// hostname completion deterministic — one spelling per host, since the hosts
// block is what shells complete from. DNS resolution is unaffected either way;
// the responder answers both spellings regardless.
func TestHostsLineNativeDNS(t *testing.T) {
	e := hostsEntry{IP: "198.18.1.5", Alias: "a.skynet", FQDN: "a.tail.ts.net", Tailnet: "skynet"}
	if line := hostsLine(e); strings.Contains(line, "a.tail.ts.net") {
		t.Errorf("native off: FQDN must stay off the line: %q", line)
	}
	e.Native = true
	if line := hostsLine(e); !strings.Contains(line, "a.tail.ts.net") {
		t.Errorf("native on: FQDN missing from line: %q", line)
	}
}
