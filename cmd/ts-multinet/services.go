// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"sort"
	"strings"

	"tailscale.com/tailcfg"
)

// Advertised Tailscale VIP services are first-class selectable resources at
// parity with peer hosts: the canonical "svc:<label>" name is what config and
// the CLI/UI store, and the service resolves under both the MagicDNS spelling
// (<label>.<suffix>) and the friendly alias (<label>.<domain>).
//
// Services are not identity-pinned the way peers are: the svc: name is the
// stable identity, and a service that vanishes surfaces like a missing peer.

// serviceLabel returns the DNS label part of a "svc:<label>" service name.
func serviceLabel(name tailcfg.ServiceName) string {
	return strings.TrimPrefix(strings.ToLower(string(name)), "svc:")
}

// serviceFQDN is a service's MagicDNS name under a tailnet's real suffix — the
// canonical name the synthetic IP is keyed on (peers and services never share
// one unless they'd share a name, which the tailnet forbids).
func serviceFQDN(name tailcfg.ServiceName, suffix string) string {
	label := serviceLabel(name)
	if suffix == "" {
		return label
	}
	return label + "." + suffix
}

// isServiceResource reports whether a config selection names a service.
func isServiceResource(name string) bool {
	return strings.HasPrefix(name, "svc:")
}

// liveServices is the production service discovery: this node's view of the
// advertised VIP services it may reach (ACL-gated by the control plane).
func liveServices(ctx context.Context, tn *Tailnet) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
	if tn.lc == nil {
		return nil, nil
	}
	return tn.lc.GetServices(ctx)
}

// serviceNames returns the svc: names of a service map, sorted.
func serviceNames(services map[tailcfg.ServiceName]tailcfg.ServiceDetails) []string {
	out := make([]string, 0, len(services))
	for name := range services {
		out = append(out, string(name))
	}
	sort.Strings(out)
	return out
}

// serviceJSON is one advertised service in the /services reply.
type serviceJSON struct {
	Name        string   `json:"name"`         // canonical svc:<label>
	DisplayName string   `json:"display_name"` // human label; falls back to the name
	VIPs        []string `json:"vips"`         // service addresses
	Ports       []string `json:"ports"`        // advertised proto:port metadata (never probed)
	Selected    bool     `json:"selected"`     // present in this tailnet's resources
}

type tailnetServicesJSON struct {
	Name     string        `json:"name"`
	Suffix   string        `json:"suffix"`
	Services []serviceJSON `json:"services"`
}
