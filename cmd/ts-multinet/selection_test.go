// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go4.org/mem"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// nodeKey builds a deterministic, distinct key.NodePublic for map slots —
// the raw-bytes constructor is the only way to mint arbitrary keys off-package.
func nodeKey(n byte) key.NodePublic {
	var raw [32]byte
	raw[0] = n
	return key.NodePublicFromRaw32(mem.B(raw[:]))
}

func mkStatus(peers ...*ipnstate.PeerStatus) *ipnstate.Status {
	st := &ipnstate.Status{BackendState: "Running", Peer: map[key.NodePublic]*ipnstate.PeerStatus{}}
	for i, p := range peers {
		st.Peer[nodeKey(byte(i+1))] = p
	}
	return st
}

func peer(id, dnsName, ip string) *ipnstate.PeerStatus {
	return &ipnstate.PeerStatus{ID: tailcfg.StableNodeID(id), DNSName: dnsName, TailscaleIPs: []netip.Addr{netip.MustParseAddr(ip)}}
}

const skynetSuffix = "tail523555.ts.net"

func TestIsMullvad(t *testing.T) {
	cases := map[string]bool{
		"us-den-wg-205.mullvad.ts.net.": true,
		"US-DEN-WG-205.MULLVAD.TS.NET":  true,
		"nucbox.tail523555.ts.net.":     false,
		"mullvad.ts.net":                false, // the zone itself is not a peer
		"":                              false,
	}
	for name, want := range cases {
		if got := isMullvad(name); got != want {
			t.Errorf("isMullvad(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPeerShort(t *testing.T) {
	if got := peerShort("Nucbox.tail523555.ts.net.", skynetSuffix); got != "nucbox" {
		t.Errorf("peerShort = %q, want nucbox", got)
	}
	if got := peerShort("nucbox.other.ts.net.", skynetSuffix); got != "" {
		t.Errorf("peerShort for foreign suffix = %q, want empty", got)
	}
	if got := peerShort("", skynetSuffix); got != "" {
		t.Errorf("peerShort for empty name = %q, want empty", got)
	}
}

func TestResolveSelectionsPinsAndReResolves(t *testing.T) {
	conf := TailnetConf{Name: "skynet", Resources: []string{"nucbox"}}
	st := mkStatus(
		peer("node-one", "nucbox."+skynetSuffix+".", "100.64.0.10"),
		peer("node-two", "other."+skynetSuffix+".", "100.64.0.11"),
	)

	pins := pinStore{}
	resolved, errs, changed := resolveSelections(skynetSuffix, conf, st, nil, pins)
	if len(errs) != 0 || len(resolved) != 1 || !changed {
		t.Fatalf("resolved=%v errs=%v changed=%v", resolved, errs, changed)
	}
	r := resolved[0]
	if r.Short != "nucbox" || r.FQDN != "nucbox."+skynetSuffix || r.Target != "node-one" || r.Addr != "100.64.0.10" {
		t.Fatalf("resolved record = %+v", r)
	}
	if pins["nucbox"].Target != "node-one" {
		t.Fatalf("pin not recorded: %+v", pins)
	}

	// Second run against the same peer: stable, no churn.
	_, errs, changed = resolveSelections(skynetSuffix, conf, st, nil, pins)
	if len(errs) != 0 || changed {
		t.Fatalf("re-resolve should be a no-op: errs=%v changed=%v", errs, changed)
	}
}

func TestResolveSelectionsMissingPeerIsLoud(t *testing.T) {
	conf := TailnetConf{Name: "skynet", Resources: []string{"ghost"}}
	st := mkStatus(peer("node-one", "nucbox."+skynetSuffix+".", "100.64.0.10"))
	resolved, errs, changed := resolveSelections(skynetSuffix, conf, st, nil, pinStore{})
	if len(resolved) != 0 || len(errs) != 1 || changed {
		t.Fatalf("resolved=%v errs=%v changed=%v", resolved, errs, changed)
	}
	if !strings.Contains(errs[0], "ghost") || !strings.Contains(errs[0], "not a peer") {
		t.Errorf("error not diagnosable: %q", errs[0])
	}
}

func TestResolveSelectionsIdentityChangeRejected(t *testing.T) {
	conf := TailnetConf{Name: "skynet", Resources: []string{"nucbox"}}
	st := mkStatus(peer("node-two", "nucbox."+skynetSuffix+".", "100.64.0.99"))
	pins := pinStore{"nucbox": {FQDN: "nucbox." + skynetSuffix, Target: "node-one", Addr: "100.64.0.10"}}

	resolved, errs, changed := resolveSelections(skynetSuffix, conf, st, nil, pins)
	if len(resolved) != 0 || len(errs) != 1 || changed {
		t.Fatalf("resolved=%v errs=%v changed=%v", resolved, errs, changed)
	}
	if !strings.Contains(errs[0], "identity changed") {
		t.Errorf("error not diagnosable: %q", errs[0])
	}
}

func TestResolveSelectionsRenameFollowsNode(t *testing.T) {
	conf := TailnetConf{Name: "skynet", Resources: []string{"renamed"}}
	st := mkStatus(peer("node-one", "renamed."+skynetSuffix+".", "100.64.0.10"))
	pins := pinStore{"renamed": {FQDN: "oldname." + skynetSuffix, Target: "node-one", Addr: "100.64.0.10"}}

	resolved, errs, changed := resolveSelections(skynetSuffix, conf, st, nil, pins)
	if len(errs) != 0 || len(resolved) != 1 || !changed {
		t.Fatalf("resolved=%v errs=%v changed=%v", resolved, errs, changed)
	}
	if pins["renamed"].FQDN != "renamed."+skynetSuffix {
		t.Errorf("pin did not follow the node: %+v", pins["renamed"])
	}
}

func TestResolveSelectionsAllowAllSkipsMullvad(t *testing.T) {
	conf := TailnetConf{Name: "skynet", AllowAll: true}
	st := mkStatus(
		peer("node-one", "nucbox."+skynetSuffix+".", "100.64.0.10"),
		peer("node-m", "us-den-wg-205.mullvad.ts.net.", "100.104.130.39"),
		peer("node-two", "zombie."+skynetSuffix+".", "100.64.0.11"),
	)
	resolved, errs, _ := resolveSelections(skynetSuffix, conf, st, nil, pinStore{})
	if len(errs) != 0 || len(resolved) != 2 {
		t.Fatalf("resolved=%v errs=%v — Mullvad peer must be excluded", resolved, errs)
	}
	for _, r := range resolved {
		if strings.Contains(r.FQDN, "mullvad") {
			t.Errorf("Mullvad peer selected: %+v", r)
		}
	}
}

func TestPinStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pins := pinStore{"nucbox": {FQDN: "nucbox." + skynetSuffix, Target: "node-one", Addr: "100.64.0.10"}}
	if err := savePins(dir, pins); err != nil {
		t.Fatal(err)
	}
	got, err := loadPins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got["nucbox"] != pins["nucbox"] {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	// Missing dir is an empty store, not an error.
	empty, err := loadPins(filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("missing pins dir: %v %v", empty, err)
	}
}

func TestLoadConfigSelectionsAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{
		"hosts_file": "/tmp/hosts-test",
		"tailnets": [
			{"name": "skynet", "cidr": "198.18.1.0/24", "tun": "tsm0", "resources": ["nucbox"]},
			{"name": "corp", "cidr": "198.18.2.0/24", "tun": "tsm1", "enabled": false, "allow_all": true}
		]
	}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tailnets[0].enabled() {
		t.Error("enabled must default to true")
	}
	if cfg.Tailnets[0].Resources[0] != "nucbox" {
		t.Errorf("resources not parsed: %+v", cfg.Tailnets[0])
	}
	if cfg.Tailnets[1].enabled() || !cfg.Tailnets[1].AllowAll {
		t.Errorf("explicit enabled=false / allow_all=true not honored: %+v", cfg.Tailnets[1])
	}
	if cfg.HostsFile != "/tmp/hosts-test" {
		t.Errorf("hosts_file not parsed: %q", cfg.HostsFile)
	}

	// authkey_env is no longer a thing; name/cidr/tun still are.
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"tailnets":[{"name":"x","cidr":"198.18.1.0/24"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(bad); err == nil || !strings.Contains(err.Error(), "tun") {
		t.Errorf("missing tun must fail: %v", err)
	}
}
