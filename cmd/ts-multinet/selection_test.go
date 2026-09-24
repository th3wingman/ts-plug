// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"go4.org/mem"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
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

// taggedPeer is a peer carrying ACL tags, as the netmap delivers them.
func taggedPeer(id, dnsName, ip string, tags ...string) *ipnstate.PeerStatus {
	p := peer(id, dnsName, ip)
	v := views.SliceOf(tags)
	p.Tags = &v
	return p
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

// Auto rules resolve like selections but stay rules in the config: tag:
// grabs peers carrying the ACL tag (identity-pinned like any selection),
// svc: grabs advertised services by label glob. A rule matching nothing is
// silent — the point is that the tag or service can appear later — while an
// explicit missing svc: entry still fails loudly.
func TestResolveSelectionsAutoRules(t *testing.T) {
	conf := TailnetConf{
		Name:       "skynet",
		Resources:  []string{"svc:my-db"}, // explicit entry: dedups against the same rule match
		AutoSelect: []string{"tag:infra", "svc:prod-*"},
	}
	st := mkStatus(
		taggedPeer("node-one", "nucbox."+skynetSuffix+".", "100.64.0.10", "tag:infra", "tag:other"),
		peer("node-two", "plain."+skynetSuffix+".", "100.64.0.11"),                           // untagged
		taggedPeer("node-m", "us-den-wg-205.mullvad.ts.net.", "100.104.130.39", "tag:infra"), // Mullvad: never selectable
	)
	svcs := map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:my-db":      {Name: "svc:my-db"},
		"svc:prod-db":    {Name: "svc:prod-db", Addrs: []netip.Addr{netip.MustParseAddr("100.64.0.50")}},
		"svc:prod-cache": {Name: "svc:prod-cache"},
		"svc:web":        {Name: "svc:web"},
	}

	pins := pinStore{}
	resolved, errs, changed := resolveSelections(skynetSuffix, conf, st, svcs, pins)
	if len(errs) != 0 {
		t.Fatalf("errs=%v — a rule matching nothing must be silent", errs)
	}
	if !changed {
		t.Fatal("first resolve must pin the tag-matched peer")
	}
	var shorts []string
	for _, r := range resolved {
		shorts = append(shorts, r.Short)
	}
	sort.Strings(shorts)
	want := []string{"nucbox", "svc:my-db", "svc:prod-cache", "svc:prod-db"}
	if !slices.Equal(shorts, want) {
		t.Fatalf("resolved = %v, want %v (deduped, Mullvad excluded, svc:web unmatched)", shorts, want)
	}
	if pins["nucbox"].Target != "node-one" {
		t.Fatalf("tag-matched peer must be identity-pinned like an explicit selection: %+v", pins)
	}
	if _, pinned := pins["svc:prod-db"]; pinned {
		t.Fatal("services are never pinned")
	}

	// An explicit svc: entry that does not exist is still loud — a typo in
	// resources is an error; a pattern matching nothing is a feature.
	conf2 := TailnetConf{Name: "skynet", Resources: []string{"svc:ghost"}, AutoSelect: []string{"svc:prod-*"}}
	_, errs, _ = resolveSelections(skynetSuffix, conf2, st, svcs, pins)
	if len(errs) != 1 || !strings.Contains(errs[0], "svc:ghost") {
		t.Fatalf("explicit missing service must error: %v", errs)
	}
}

func TestNormalizeAutoRule(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"tag:infra", "tag:infra"},
		{"TAG:Infra", "tag:infra"},
		{" svc:prod-* ", "svc:prod-*"},
		{"SVC:Prod-DB", "svc:prod-db"},
		{"svc:db", "svc:db"},
		{"tcp:3306", "tcp:3306"},
		{"UDP:53", "udp:53"},
		{"tcp:3300-3400", "tcp:3300-3400"},
		{"tcp:*", "tcp:*"},
	} {
		got, err := normalizeAutoRule(c.in)
		if err != nil || got != c.want {
			t.Errorf("normalizeAutoRule(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "prod", "svc:", "tag:", "svc:[", "tcp:", "tcp:0", "tcp:abc"} {
		if _, err := normalizeAutoRule(bad); err == nil {
			t.Errorf("normalizeAutoRule(%q) accepted a malformed rule", bad)
		}
	}
}

// portRuleMatches is the "allow all mysql" matcher: the rule must be fully
// covered by an advertised proto:port range — proto has to agree, and a
// wider rule must not overclaim against a narrower advertised range.
func TestPortRuleMatches(t *testing.T) {
	ranges := func(first, last uint16) []tailcfg.ProtoPortRange {
		return []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: first, Last: last}}}
	}
	if !portRuleMatches("tcp:3306", ranges(3306, 3306)) {
		t.Error("exact port must match")
	}
	if !portRuleMatches("tcp:3306", ranges(3300, 3400)) {
		t.Error("rule inside an advertised range must match")
	}
	if portRuleMatches("tcp:3300-3400", ranges(3306, 3306)) {
		t.Error("a rule wider than the advertised range must not overclaim")
	}
	if portRuleMatches("udp:3306", ranges(3306, 3306)) {
		t.Error("proto must agree")
	}
	if portRuleMatches("tcp:bad", ranges(3306, 3306)) {
		t.Error("a malformed rule must never match")
	}
}

// Port rules select by what a service advertises — the PORTS column is the
// only "type" Tailscale exposes: tcp:3306 grabs every MySQL, tcp:22 every
// SSH, and a service covering a whole range satisfies a rule inside it.
func TestResolveSelectionsAutoPortRules(t *testing.T) {
	ranges := func(first, last uint16) []tailcfg.ProtoPortRange {
		return []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: first, Last: last}}}
	}
	conf := TailnetConf{Name: "skynet", AutoSelect: []string{"tcp:3306", "tcp:22", "tcp:3300-3400"}}
	st := mkStatus(peer("node-one", "nucbox."+skynetSuffix+".", "100.64.0.10"))
	svcs := map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:mysql":  {Name: "svc:mysql", Ports: ranges(3306, 3306)},
		"svc:sshd":   {Name: "svc:sshd", Ports: ranges(22, 22)},
		"svc:pgpool": {Name: "svc:pgpool", Ports: ranges(3300, 3400)},
		"svc:web":    {Name: "svc:web", Ports: ranges(80, 80)},
	}

	resolved, errs, _ := resolveSelections(skynetSuffix, conf, st, svcs, pinStore{})
	if len(errs) != 0 {
		t.Fatalf("errs=%v — a port rule matching nothing must be silent", errs)
	}
	var shorts []string
	for _, r := range resolved {
		shorts = append(shorts, r.Short)
	}
	sort.Strings(shorts)
	want := []string{"svc:mysql", "svc:pgpool", "svc:sshd"}
	if !slices.Equal(shorts, want) {
		t.Fatalf("resolved = %v, want %v (pgpool's range covers tcp:3300-3400; svc:web is tcp:80)", shorts, want)
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
