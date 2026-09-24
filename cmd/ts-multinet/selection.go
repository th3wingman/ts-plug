// Copyright (c) Tailscale Inc & Authors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/json"
	"fmt"
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
	// adds every peer carrying the ACL tag (walking the same Mullvad-filtered,
	// short-name index the explicit selections use), and service rules add
	// every advertised service they cover. A peer tagged later, a service
	// advertised later — or a tag removed / a service withdrawn — lands on
	// the next apply, which the netmap watcher triggers live. Rule-matched
	// peers ride the same pin machinery as explicit selections, so identity
	// changes still fail loudly instead of redirecting.
	for _, rule := range conf.AutoSelect {
		if strings.HasPrefix(rule, "tag:") {
			for _, short := range order { // netmap order, same as the index
				if peerHasTag(byShort[short], rule) && !slices.Contains(want, short) {
					want = append(want, short)
				}
			}
		}
	}
	// Service rules: svc: globs the label, tcp:/udp: selects every service
	// whose advertised ports cover the rule — the PORTS column is the closest
	// thing to a service "type" Tailscale exposes (the official CLI infers
	// ssh/database types from these same ports).
	for _, name := range serviceNames(services) { // sorted
		if slices.Contains(want, name) {
			continue
		}
		if autoSvcMatched(conf.AutoSelect, name, services[tailcfg.ServiceName(name)]) {
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
// addresses, actions — nothing tag-like), so service "tags" are a naming
// convention: an svc: rule is a glob over the service label. Peers do carry
// real ACL tags, so tag: rules match those exactly. Rules never validate
// against what exists right now — the whole point is that a service
// advertised later or a device tagged later lands without touching the
// config; the control API reports what they currently match instead.

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
	case strings.HasPrefix(r, "tag:"):
		if len(r) <= len("tag:") {
			return "", fmt.Errorf("auto_select %q: empty tag", rule)
		}
	case strings.HasPrefix(r, "tcp:"), strings.HasPrefix(r, "udp:"):
		if _, err := tailcfg.ParseProtoPortRanges([]string{r}); err != nil {
			return "", fmt.Errorf("auto_select %q: bad proto:port (%v)", rule, err)
		}
	default:
		return "", fmt.Errorf("auto_select %q: must start with svc: (service label glob), tag: (peer ACL tag), or tcp:/udp: (advertised port)", rule)
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

// peerHasTag reports whether a peer carries an ACL tag (nil-safe — the
// status carries tags as a pointer to a read-only view).
func peerHasTag(p *ipnstate.PeerStatus, tag string) bool {
	return p != nil && p.Tags != nil && p.Tags.ContainsFunc(func(t string) bool { return t == tag })
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

// autoTagMatched reports whether any tag: auto rule selects this peer —
// the peers listing's server-side Selected flag (the same effective union
// resolveSelections applies).
func autoTagMatched(rules []string, p *ipnstate.PeerStatus) bool {
	for _, rule := range rules {
		if strings.HasPrefix(rule, "tag:") && peerHasTag(p, rule) {
			return true
		}
	}
	return false
}

// autoSvcMatched reports whether any svc: or tcp:/udp: auto rule selects an
// advertised service: svc: globs the label, a port rule matches when one of
// the service's advertised proto:port ranges fully covers it.
func autoSvcMatched(rules []string, svcName string, sd tailcfg.ServiceDetails) bool {
	for _, rule := range rules {
		switch {
		case strings.HasPrefix(rule, "svc:"):
			if svcRuleMatches(rule, svcName) {
				return true
			}
		case strings.HasPrefix(rule, "tcp:"), strings.HasPrefix(rule, "udp:"):
			if portRuleMatches(rule, sd.Ports) {
				return true
			}
		}
	}
	return false
}
