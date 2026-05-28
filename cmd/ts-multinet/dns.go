// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/miekg/dns"
)

// registry is the shared brain. The DNS responder allocates a synthetic IP per
// (tailnet, fqdn) and records the reverse mapping; the per-tailnet forwarder
// looks the IP back up to know what hostname to dial.
//
// Synthetic ranges are disjoint per tailnet, so a single net.IP uniquely
// identifies both the tailnet and the original name.
type registry struct {
	mu     sync.Mutex
	suffix []suffixEntry         // longest-first, for most-specific match
	allocs map[string]*allocator // tailnet name -> allocator
	byName map[string]net.IP     // fqdn -> synthetic IP
	byIP   map[string]string     // synthetic IP string -> fqdn
	cidrs  []*net.IPNet          // all synthetic ranges, for loop guard
}

type suffixEntry struct {
	suffix  string // e.g. "skynet.ts.net"
	tailnet string
}

func newRegistry(tailnets []TailnetConf) (*registry, error) {
	r := &registry{
		allocs: make(map[string]*allocator),
		byName: make(map[string]net.IP),
		byIP:   make(map[string]string),
	}
	for _, tc := range tailnets {
		a, err := newAllocator(tc.CIDR)
		if err != nil {
			return nil, fmt.Errorf("tailnet %q: %w", tc.Name, err)
		}
		r.allocs[tc.Name] = a
		if _, ipnet, err := net.ParseCIDR(tc.CIDR); err == nil {
			r.cidrs = append(r.cidrs, ipnet)
		}
		// A suffix from config is registered immediately; an empty one is
		// auto-detected and registered after the tailnet comes up.
		if s := strings.TrimSuffix(strings.ToLower(tc.Suffix), "."); s != "" {
			r.suffix = append(r.suffix, suffixEntry{suffix: s, tailnet: tc.Name})
		}
	}
	r.sortSuffix()
	return r, nil
}

// registerSuffix records (or replaces) the MagicDNS suffix for a tailnet,
// typically discovered from tsnet status after bring-up.
func (r *registry) registerSuffix(tailnet, suffix string) {
	suffix = strings.TrimSuffix(strings.ToLower(suffix), ".")
	if suffix == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, se := range r.suffix {
		if se.tailnet == tailnet {
			r.suffix[i].suffix = suffix
			r.sortSuffix()
			return
		}
	}
	r.suffix = append(r.suffix, suffixEntry{suffix: suffix, tailnet: tailnet})
	r.sortSuffix()
}

// sortSuffix orders longest-first for most-specific match. Caller holds the
// lock (or is in single-threaded setup).
func (r *registry) sortSuffix() {
	sort.Slice(r.suffix, func(i, j int) bool {
		return len(r.suffix[i].suffix) > len(r.suffix[j].suffix)
	})
}

// match reports which tailnet a name belongs to, if any.
func (r *registry) match(name string) (tailnet string, ok bool) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, se := range r.suffix {
		if name == se.suffix || strings.HasSuffix(name, "."+se.suffix) {
			return se.tailnet, true
		}
	}
	return "", false
}

// allocate returns a stable synthetic IP for a name belonging to a tailnet,
// minting one on first use. Returns false if the name isn't ours or the range
// is exhausted.
func (r *registry) allocate(name string) (net.IP, bool) {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	tailnet, ok := r.match(name)
	if !ok {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ip, ok := r.byName[name]; ok {
		return ip, true
	}
	ip, ok := r.allocs[tailnet].take()
	if !ok {
		return nil, false
	}
	r.byName[name] = ip
	r.byIP[ip.String()] = name
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

	if _, mine := d.reg.match(q.Name); mine {
		m := new(dns.Msg)
		m.SetReply(req)
		m.Authoritative = true
		// Only A is synthesized; AAAA (and anything else) returns an empty
		// NOERROR so resolvers fall back to the A record.
		if q.Qtype == dns.TypeA {
			if ip, ok := d.reg.allocate(q.Name); ok {
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
