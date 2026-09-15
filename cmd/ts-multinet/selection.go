// Copyright (c) Tailscale Inc & Authors
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	want := make([]string, 0, len(conf.Resources))
	if conf.AllowAll {
		want = append(want, order...)
		sort.Strings(want)
	} else {
		want = append(want, conf.Resources...)
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
