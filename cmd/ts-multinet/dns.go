// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// registry is the shared brain. The DNS responder allocates a synthetic IP per
// (tailnet, fqdn) and records the reverse mapping; the per-tailnet forwarder
// looks the IP back up to know what hostname to dial.
//
// Synthetic ranges are disjoint per tailnet, so a single net.IP uniquely
// identifies both the tailnet and the original name.
type registry struct {
	mu        sync.Mutex
	entries   []*tnEntry            // per tailnet
	byName    map[string]net.IP     // real fqdn -> synthetic IP
	byIP      map[string]string     // synthetic IP string -> real fqdn
	cidrs     []*net.IPNet          // all synthetic ranges, for loop guard
	resolvers map[string]resolveFunc // tailnet -> peer-list resolver (existence check)
}

type tnEntry struct {
	name   string // friendly name, e.g. "skynet" — also accepted as an alias suffix
	suffix string // real MagicDNS suffix, e.g. "tail523555.ts.net"; "" until detected
	alloc  *allocator
}

func newRegistry(tailnets []TailnetConf) (*registry, error) {
	r := &registry{
		byName:    make(map[string]net.IP),
		byIP:      make(map[string]string),
		resolvers: make(map[string]resolveFunc),
	}
	for _, tc := range tailnets {
		a, err := newAllocator(tc.CIDR)
		if err != nil {
			return nil, fmt.Errorf("tailnet %q: %w", tc.Name, err)
		}
		if _, ipnet, err := net.ParseCIDR(tc.CIDR); err == nil {
			r.cidrs = append(r.cidrs, ipnet)
		}
		r.entries = append(r.entries, &tnEntry{
			name:   strings.ToLower(tc.Name),
			suffix: strings.TrimSuffix(strings.ToLower(tc.Suffix), "."),
			alloc:  a,
		})
	}
	return r, nil
}

// registerSuffix records the real MagicDNS suffix for a tailnet, typically
// discovered from tsnet status after bring-up.
func (r *registry) registerSuffix(tailnet, suffix string) {
	suffix = strings.TrimSuffix(strings.ToLower(suffix), ".")
	if suffix == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.name == strings.ToLower(tailnet) {
			e.suffix = suffix
			return
		}
	}
}

// registerResolver wires a tailnet's peer-list resolver so DNS can verify a
// host actually exists before answering (NXDOMAIN otherwise → search fallthrough).
func (r *registry) registerResolver(tailnet string, fn resolveFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolvers[strings.ToLower(tailnet)] = fn
}

// match resolves a queried name to its tailnet and canonical real FQDN. It
// accepts the real MagicDNS suffix (host.tail523555.ts.net) and the friendly
// alias (host.skynet -> host.tail523555.ts.net). Longest match wins.
func (r *registry) match(name string) (tailnet, realFQDN string, ok bool) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	r.mu.Lock()
	defer r.mu.Unlock()

	bestLen := -1
	for _, e := range r.entries {
		// Real suffix: the name is already canonical.
		if e.suffix != "" && (name == e.suffix || strings.HasSuffix(name, "."+e.suffix)) {
			if len(e.suffix) > bestLen {
				bestLen, tailnet, realFQDN, ok = len(e.suffix), e.name, name, true
			}
		}
		// Friendly alias: only once we know the real suffix to canonicalize to.
		if e.suffix != "" && (name == e.name || strings.HasSuffix(name, "."+e.name)) {
			short := strings.TrimSuffix(name, "."+e.name)
			if short == name { // name == e.name exactly
				short = ""
			}
			real := e.suffix
			if short != "" {
				real = short + "." + e.suffix
			}
			if len(e.name) > bestLen {
				bestLen, tailnet, realFQDN, ok = len(e.name), e.name, real, true
			}
		}
	}
	return tailnet, realFQDN, ok
}

// exists checks whether realFQDN is a real peer on the tailnet. If no resolver
// is registered yet (startup race), it allows the name through.
func (r *registry) exists(ctx context.Context, tailnet, realFQDN string) bool {
	r.mu.Lock()
	fn := r.resolvers[tailnet]
	r.mu.Unlock()
	if fn == nil {
		return true
	}
	_, ok := fn(ctx, realFQDN)
	return ok
}

// locate resolves a possibly-bare/alias/fqdn name to the tailnet + canonical
// real FQDN of an existing peer. Used by `check` (which, unlike a libc resolver,
// gets the raw arg with no search-list expansion). Bare names are tried against
// each tailnet in config order; first existing wins.
func (r *registry) locate(ctx context.Context, name string) (tailnet, realFQDN string, ok bool) {
	if t, real, m := r.match(name); m {
		if r.exists(ctx, t, real) {
			return t, real, true
		}
		return "", "", false // named a tailnet, but the host isn't a peer there
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	r.mu.Lock()
	type cand struct{ t, real string }
	var cands []cand
	for _, e := range r.entries {
		if e.suffix != "" {
			cands = append(cands, cand{e.name, name + "." + e.suffix})
		}
	}
	r.mu.Unlock()
	for _, c := range cands {
		if r.exists(ctx, c.t, c.real) {
			return c.t, c.real, true
		}
	}
	return "", "", false
}

// allocate returns a stable synthetic IP for a canonical real FQDN, minting one
// on first use. Repeated aliases of the same host share one IP.
func (r *registry) allocate(tailnet, realFQDN string) (net.IP, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ip, ok := r.byName[realFQDN]; ok {
		return ip, true
	}
	var alloc *allocator
	for _, e := range r.entries {
		if e.name == tailnet {
			alloc = e.alloc
			break
		}
	}
	if alloc == nil {
		return nil, false
	}
	ip, ok := alloc.take()
	if !ok {
		return nil, false
	}
	r.byName[realFQDN] = ip
	r.byIP[ip.String()] = realFQDN
	return ip, true
}

// isSynthetic reports whether ip falls in any tailnet's synthetic range.
func (r *registry) isSynthetic(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range r.cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// lookup maps a synthetic IP back to its fqdn for the forwarder.
func (r *registry) lookup(ip net.IP) (fqdn string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fqdn, ok = r.byIP[ip.String()]
	return
}

// allocator hands out sequential host addresses from a CIDR, skipping the
// network address and leaving the broadcast address alone.
type allocator struct {
	base uint32 // network address, host byte order
	next uint32 // next host offset to hand out
	size uint32 // total addresses in the range
}

func newAllocator(cidr string) (*allocator, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("cidr %q is not IPv4", cidr)
	}
	ones, bits := ipnet.Mask.Size()
	return &allocator{
		base: binary.BigEndian.Uint32(ip4.Mask(ipnet.Mask)),
		next: 1, // skip .0
		size: uint32(1) << uint(bits-ones),
	}, nil
}

func (a *allocator) take() (net.IP, bool) {
	if a.next >= a.size-1 { // leave the last (broadcast) address
		return nil, false
	}
	v := a.base + a.next
	a.next++
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, v)
	return ip, true
}

// dnsServer answers tailnet names with synthetic IPs and forwards everything
// else to the inherited upstream resolver.
type dnsServer struct {
	reg      *registry
	upstream string
	client   dns.Client
}

func (d *dnsServer) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) == 0 {
		dns.HandleFailed(w, req)
		return
	}
	q := req.Question[0]

	if tailnet, realFQDN, mine := d.reg.match(q.Name); mine {
		m := new(dns.Msg)
		m.SetReply(req)
		m.Authoritative = true

		// Only answer if the host is a real peer. NXDOMAIN otherwise, so a
		// resolver walking its search list falls through to the next tailnet.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !d.reg.exists(ctx, tailnet, realFQDN) {
			m.Rcode = dns.RcodeNameError // NXDOMAIN
			_ = w.WriteMsg(m)
			return
		}

		// A is synthesized; AAAA (and anything else) returns empty NOERROR so
		// resolvers fall back to the A record.
		if q.Qtype == dns.TypeA {
			if ip, ok := d.reg.allocate(tailnet, realFQDN); ok {
				rr, err := dns.NewRR(fmt.Sprintf("%s 1 IN A %s", q.Name, ip.String()))
				if err == nil {
					m.Answer = append(m.Answer, rr)
				}
			}
		}
		_ = w.WriteMsg(m)
		return
	}

	resp, _, err := d.client.Exchange(req, d.upstream)
	if err != nil || resp == nil {
		dns.HandleFailed(w, req)
		return
	}
	_ = w.WriteMsg(resp)
}

func startDNS(listen string, ds *dnsServer) error {
	if listen == "" {
		listen = "127.0.0.1:53"
	}
	srv := &dns.Server{Addr: listen, Net: "udp", Handler: ds}
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			slog.Error("dns ListenAndServe", "err", err)
		}
	}()
	return nil
}
