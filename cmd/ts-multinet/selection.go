// Copyright (c) Tailscale Inc & Authors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

// selectedResource is one config selection resolved against a tailnet's live
// peer list. Adopted from Cauldron's tailnet-resource model: the stable peer
// ID is captured so a later rename or replacement can never silently redirect
// an entry that was already granted.
type selectedResource struct {
	Short  string `json:"short"`  // friendly alias, e.g. "nucbox"
	FQDN   string `json:"fqdn"`   // canonical MagicDNS name, e.g. "nucbox.tail523555.ts.net"
	Target string `json:"target"` // stable peer ID (node key) — the identity pin
	Addr   string `json:"addr"`   // real tailnet 100.x at pin time
}

// pinStore persists selection pins per tailnet so identity changes are caught
// across daemon restarts, not just within one run.
type pinStore map[string]pinRecord // short name -> pin

type pinRecord struct {
	FQDN   string `json:"fqdn"`
	Target string `json:"target"`
	Addr   string `json:"addr"`
}

func pinsPath(stateDir string) string {
	return filepath.Join(stateDir, "selections.json")
}

func loadPins(stateDir string) (pinStore, error) {
	b, err := os.ReadFile(pinsPath(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return pinStore{}, nil
		}
		return nil, err
	}
	var p pinStore
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", pinsPath(stateDir), err)
	}
	return p, nil
}

func savePins(stateDir string, p pinStore) error {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pinsPath(stateDir), b, 0600)
}

// isMullvad reports whether a peer is one of Tailscale's shared Mullvad exit
// peers. They ride along in every tailnet with the Mullvad add-on and are
// transit infrastructure, not resources — excluded from selection, allow_all,
// and listings (Cauldron filters them for the same reason).
func isMullvad(dnsName string) bool {
	name := strings.TrimSuffix(strings.ToLower(dnsName), ".")
	return strings.HasSuffix(name, ".mullvad.ts.net")
}

// peerShort returns the short alias of a peer relative to its tailnet suffix,
// or "" when the peer has no DNS name in that suffix.
func peerShort(dnsName, suffix string) string {
	if dnsName == "" {
		return ""
	}
	name := strings.TrimSuffix(strings.ToLower(dnsName), ".")
	if suffix == "" {
		return name
	}
	if !strings.HasSuffix(name, "."+suffix) {
		return ""
	}
	return strings.TrimSuffix(name, "."+suffix)
}

// resolveSelections maps a tailnet's configured selections (explicit resources
// plus allow_all) to pinned peers from the live status, and to advertised
// services from the live service list. Missing peers, missing services, and
// identity mismatches are reported as errors and skipped loudly — never
// silently redirected. Returns the resolved set, the errors, and whether any
// pin was added or updated (caller persists on change). Services are not
// pinned: their svc: name is the identity, so a vanished one surfaces like a
// missing peer.
func resolveSelections(suffix string, conf TailnetConf, st *ipnstate.Status, services map[tailcfg.ServiceName]tailcfg.ServiceDetails, pins pinStore) (resolved []selectedResource, errs []string, changed bool) {
	if st == nil {
		return nil, []string{"no peer status"}, false
	}
	// Index peers by short alias. First match wins; collisions across
	// long-tail DNS names are possible and resolve in netmap order.
	byShort := map[string]*ipnstate.PeerStatus{}
	var order []string
	for _, p := range st.Peer {
		if isMullvad(p.DNSName) {
			continue
		}
		s := peerShort(p.DNSName, suffix)
		if s == "" {
			continue
		}
		if _, dup := byShort[s]; !dup {
			order = append(order, s)
			byShort[s] = p
		}
	}

	want := make([]string, 0, len(conf.Resources)+len(conf.Locked))
	if conf.AllowAll {
		want = append(want, order...)
		sort.Strings(want)
	} else {
		want = append(want, conf.Resources...)
	}
	// Locked selections are always selected: every daemon write keeps
	// locked ⊆ resources, and this also self-heals a hand-edited config
	// that lists a lock without the matching resource.
	for _, l := range conf.Locked {
		if !slices.Contains(want, l) {
			want = append(want, l)
		}
	}

	// Auto rules expand at resolve time and never touch the config: tag:
	// adds every peer carrying a matching ACL tag and host: every peer whose short
	// name matches the glob (walking the same Mullvad-filtered, short-name
	// index the explicit selections use), and service rules add
	// every advertised service they cover. A peer tagged later, a service
	// advertised later — or a tag removed / a service withdrawn — lands on
	// the next apply, which the netmap watcher triggers live. Rule-matched
	// peers ride the same pin machinery as explicit selections, so identity
	// changes still fail loudly instead of redirecting.
	if len(conf.AutoSelect) > 0 {
		for _, short := range order { // netmap order, same as the index
			if !slices.Contains(want, short) && autoPeerRule(conf.AutoSelect, short, peerTags(byShort[short])) != "" {
				want = append(want, short)
			}
		}
	}
	// Service rules: svc: globs the label, tcp:/udp: selects every service
	// whose advertised ports cover the rule — the PORTS column is the closest
	// thing to a service "type" Tailscale exposes (the official CLI infers
	// ssh/database types from these same ports).
	hostTags := serviceHostTags(st, services)
	for _, name := range serviceNames(services) { // sorted
		if slices.Contains(want, name) {
			continue
		}
		sn := tailcfg.ServiceName(name)
		if autoSvcRule(conf.AutoSelect, name, services[sn], hostTags[sn]) != "" {
			want = append(want, name)
		}
	}

	for _, short := range want {
		if isServiceResource(short) {
			sd, ok := services[tailcfg.ServiceName(short)]
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: not an advertised service on %s (run `services %s` to see what exists)", short, conf.Name, conf.Name))
				continue
			}
			addr := ""
			if len(sd.Addrs) > 0 {
				addr = sd.Addrs[0].String()
			}
			resolved = append(resolved, selectedResource{Short: short, FQDN: serviceFQDN(sd.Name, suffix), Addr: addr})
			continue
		}
		p := byShort[short]
		if p == nil {
			errs = append(errs, fmt.Sprintf("%s: not a peer on %s (run `peers %s` to see what exists)", short, conf.Name, conf.Name))
			continue
		}
		fqdn := strings.TrimSuffix(strings.ToLower(p.DNSName), ".")
		target := string(p.ID)
		addr := ""
		if len(p.TailscaleIPs) > 0 {
			addr = p.TailscaleIPs[0].String()
		}
		if pin, ok := pins[short]; ok {
			if pin.Target != target {
				errs = append(errs, fmt.Sprintf("%s: identity changed (pinned node %s, found %s) — remove the pin in selections.json to re-select",
					short, shortTarget(pin.Target), shortTarget(target)))
				continue
			}
			// Same node, drifted name or address (e.g. MagicDNS suffix change):
			// the pin follows the node, like Cauldron's identity-keyed grants.
			if pin.FQDN != fqdn || pin.Addr != addr {
				pins[short] = pinRecord{FQDN: fqdn, Target: target, Addr: addr}
				changed = true
			}
		} else {
			pins[short] = pinRecord{FQDN: fqdn, Target: target, Addr: addr}
			changed = true
		}
		resolved = append(resolved, selectedResource{Short: short, FQDN: fqdn, Target: target, Addr: addr})
	}
	return resolved, errs, changed
}

func shortTarget(target string) string {
	if len(target) > 8 {
		return target[:8] + "…"
	}
	return target
}

// --- auto-select rules ---
//
// Tailscale exposes no tags on services (ServiceDetails carries name, ports,
// addresses, actions — nothing tag-like), so an svc: rule is a glob over the
// service label, and a tag: rule (a glob too) reaches a service through the
// ACL tags of the peers hosting it — the tags that approve a service host
// in the policy. Rules never validate
// against what exists right now — the whole point is that a service
// advertised later or a device tagged later lands without touching the
// config; the control API reports what they currently match instead. host:
// rules glob the peer's short name the same way svc: globs a service label.

// normalizeAutoRule validates one auto_select rule and returns its
// canonical (lowercased) form. A malformed rule is a config-load error, not
// a silent no-op — a typo'd glob would otherwise look like "matches nothing".
func normalizeAutoRule(rule string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(rule))
	switch {
	case strings.HasPrefix(r, "svc:"):
		label := strings.TrimPrefix(r, "svc:")
		if label == "" {
			return "", fmt.Errorf("auto_select %q: empty service label", rule)
		}
		if _, err := path.Match(label, "x"); err != nil {
			return "", fmt.Errorf("auto_select %q: bad glob: %v", rule, err)
		}
	case strings.HasPrefix(r, "host:"):
		name := strings.TrimPrefix(r, "host:")
		if name == "" {
			return "", fmt.Errorf("auto_select %q: empty host name", rule)
		}
		if _, err := path.Match(name, "x"); err != nil {
			return "", fmt.Errorf("auto_select %q: bad glob: %v", rule, err)
		}
	case strings.HasPrefix(r, "tag:"):
		if len(r) <= len("tag:") {
			return "", fmt.Errorf("auto_select %q: empty tag", rule)
		}
		if _, err := path.Match(r, "x"); err != nil {
			return "", fmt.Errorf("auto_select %q: bad glob: %v", rule, err)
		}
	case strings.HasPrefix(r, "tcp:"), strings.HasPrefix(r, "udp:"):
		if _, err := tailcfg.ParseProtoPortRanges([]string{r}); err != nil {
			return "", fmt.Errorf("auto_select %q: bad proto:port (%v)", rule, err)
		}
	default:
		return "", fmt.Errorf("auto_select %q: must start with svc: (service label glob), host: (peer name glob), tag: (ACL tag glob: peers and the services they host), or tcp:/udp: (advertised port)", rule)
	}
	return r, nil
}

// svcRuleMatches reports whether an svc: rule selects the advertised
// service name. An exact name (no wildcard) behaves like the same entry in
// resources; labels are lowercased on both sides by construction.
func svcRuleMatches(rule, svcName string) bool {
	if !strings.HasPrefix(rule, "svc:") || !isServiceResource(svcName) {
		return false
	}
	ok, err := path.Match(strings.TrimPrefix(rule, "svc:"), serviceLabel(tailcfg.ServiceName(svcName)))
	return err == nil && ok
}

// peerTags returns a peer's ACL tags (nil-safe — the status carries tags as
// a pointer to a read-only view).
func peerTags(p *ipnstate.PeerStatus) []string {
	if p == nil || p.Tags == nil {
		return nil
	}
	return p.Tags.AsSlice()
}

// portRuleMatches reports whether a "tcp:<port>" / "udp:<port>" rule (a
// single port, a first-last range, or "*") is fully covered by one of the
// service's advertised proto:port ranges. The rule must be well-formed —
// normalizeAutoRule validated it at load; a bad rule here just never matches.
func portRuleMatches(rule string, ports []tailcfg.ProtoPortRange) bool {
	want, err := tailcfg.ParseProtoPortRanges([]string{rule})
	if err != nil || len(want) != 1 {
		return false
	}
	for _, pp := range ports {
		if (want[0].Proto == 0 || pp.Proto == 0 || pp.Proto == want[0].Proto) &&
			pp.Ports.First <= want[0].Ports.First && want[0].Ports.Last <= pp.Ports.Last {
			return true
		}
	}
	return false
}

// autoPeerRule returns the first tag: or host: auto rule that selects the
// peer (short name, live ACL tags), or "" — shared by resolveSelections, the
// peers listing's Selected flag, the auto note and forget's refusal, so all
// four agree on what a rule matches.
func autoPeerRule(rules []string, short string, tags []string) string {
	for _, rule := range rules {
		switch {
		case strings.HasPrefix(rule, "tag:"):
			if tagRuleMatches(rule, tags) {
				return rule
			}
		case strings.HasPrefix(rule, "host:"):
			if ok, err := path.Match(strings.TrimPrefix(rule, "host:"), short); err == nil && ok {
				return rule
			}
		}
	}
	return ""
}

// autoSvcRule returns the first auto rule that selects an advertised
// service, or "": svc: globs the label, a port rule matches when one of the
// service's advertised proto:port ranges fully covers it, and tag: globs the
// ACL tags of the peers hosting it (hostTags, see serviceHostTags).
func autoSvcRule(rules []string, svcName string, sd tailcfg.ServiceDetails, hostTags []string) string {
	for _, rule := range rules {
		switch {
		case strings.HasPrefix(rule, "svc:"):
			if svcRuleMatches(rule, svcName) {
				return rule
			}
		case strings.HasPrefix(rule, "tcp:"), strings.HasPrefix(rule, "udp:"):
			if portRuleMatches(rule, sd.Ports) {
				return rule
			}
		case strings.HasPrefix(rule, "tag:"):
			if tagRuleMatches(rule, hostTags) {
				return rule
			}
		}
	}
	return ""
}

// tagRuleMatches reports whether a tag: rule (a glob over the full
// "tag:name", so tag:pam* covers tag:pam-db) matches any of the tags.
func tagRuleMatches(rule string, tags []string) bool {
	for _, t := range tags {
		if ok, err := path.Match(rule, t); err == nil && ok {
			return true
		}
	}
	return false
}

// serviceHostTags gives each advertised service the ACL tags of the peers
// hosting it. ServiceDetails carries no tags, but control routes a service
// VIP to its hosts, so a host lists the VIP in its AllowedIPs. Only exact
// single-IP entries count: an exit node's 0.0.0.0/0 or a subnet route that
// happens to cover the VIP must not tag every service.
func serviceHostTags(st *ipnstate.Status, services map[tailcfg.ServiceName]tailcfg.ServiceDetails) map[tailcfg.ServiceName][]string {
	if st == nil || len(services) == 0 {
		return nil
	}
	byVIP := map[netip.Addr][]tailcfg.ServiceName{}
	for name, sd := range services {
		for _, a := range sd.Addrs {
			byVIP[a] = append(byVIP[a], name)
		}
	}
	out := map[tailcfg.ServiceName][]string{}
	for _, p := range st.Peer {
		if isMullvad(p.DNSName) || p.Tags == nil || p.AllowedIPs == nil {
			continue
		}
		for _, pfx := range p.AllowedIPs.All() {
			if !pfx.IsSingleIP() {
				continue
			}
			for _, name := range byVIP[pfx.Addr()] {
				for _, t := range p.Tags.All() {
					if !slices.Contains(out[name], t) {
						out[name] = append(out[name], t)
					}
				}
			}
		}
	}
	return out
}
