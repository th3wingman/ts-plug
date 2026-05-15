package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/miekg/dns"
	"tailscale.com/tsnet"
)

func startDNS(ctx context.Context, ts *tsnet.Server, cfg *Config) error {
	// local names are auto-derived from routes: SNI verbatim, or the
	// host part of Upstream for raw TCP routes. Stored normalized
	// (lowercase, no trailing dot) for case-insensitive lookup.
	local := localNames(cfg.Routes)
	domain := dns.Fqdn(strings.ToLower(cfg.Domain))
	localIP := net.ParseIP(cfg.LocalIP)
	if localIP == nil {
		return fmt.Errorf("invalid local_ip %q", cfg.LocalIP)
	}

	h := &dnsHandler{
		ts:      ts,
		ctx:     ctx,
		local:   local,
		domain:  domain,
		localIP: localIP,
	}

	udpServer := &dns.Server{Addr: cfg.DNSListen, Net: "udp", Handler: h}
	tcpServer := &dns.Server{Addr: cfg.DNSListen, Net: "tcp", Handler: h}

	go func() {
		if err := udpServer.ListenAndServe(); err != nil {
			slog.Error("dns udp listener died", slog.Any("error", err))
		}
	}()
	go func() {
		if err := tcpServer.ListenAndServe(); err != nil {
			slog.Error("dns tcp listener died", slog.Any("error", err))
		}
	}()
	go func() {
		<-ctx.Done()
		_ = udpServer.Shutdown()
		_ = tcpServer.Shutdown()
	}()

	slog.Info("dns started",
		slog.String("listen", cfg.DNSListen),
		slog.String("domain", cfg.Domain),
		slog.String("local_ip", cfg.LocalIP),
		slog.Int("local_names", len(local)),
	)
	return nil
}

func localNames(routes []Route) map[string]bool {
	m := make(map[string]bool, len(routes))
	for _, r := range routes {
		var name string
		if r.SNI != "" {
			name = r.SNI
		} else {
			host, _, err := net.SplitHostPort(r.Upstream)
			if err != nil {
				name = r.Upstream
			} else {
				name = host
			}
		}
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		if name != "" {
			m[name] = true
		}
	}
	return m
}

type dnsHandler struct {
	ts      *tsnet.Server
	ctx     context.Context
	local   map[string]bool
	domain  string
	localIP net.IP
}

func (h *dnsHandler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeFormatError)
		_ = w.WriteMsg(m)
		return
	}
	q := r.Question[0]
	qname := strings.ToLower(dns.Fqdn(q.Name))

	switch q.Qtype {
	case dns.TypeA, dns.TypeAAAA:
		h.handleAddr(w, r, qname, q.Qtype)
	default:
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeNotImplemented)
		_ = w.WriteMsg(m)
	}
}

func (h *dnsHandler) handleAddr(w dns.ResponseWriter, r *dns.Msg, qname string, qtype uint16) {
	qtypeStr := dns.TypeToString[qtype]
	if !dns.IsSubDomain(h.domain, qname) {
		slog.Info("dns query", slog.String("qname", qname), slog.String("qtype", qtypeStr), slog.String("decision", "refused"))
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeRefused)
		_ = w.WriteMsg(m)
		return
	}

	bare := strings.TrimSuffix(qname, ".")
	if h.local[bare] {
		slog.Info("dns query", slog.String("qname", qname), slog.String("qtype", qtypeStr), slog.String("decision", "local"), slog.String("answer", h.localIP.String()))
		h.writeAddrAnswer(w, r, qname, qtype, []net.IP{h.localIP})
		return
	}

	ips, err := h.peerLookup(qname, qtype)
	if err != nil {
		slog.Warn("peer lookup failed", slog.String("qname", qname), slog.Any("error", err))
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}
	if len(ips) == 0 {
		slog.Info("dns query", slog.String("qname", qname), slog.String("qtype", qtypeStr), slog.String("decision", "no-data"))
		// In-domain but unknown to the tailnet. NoData is more
		// honest than NXDOMAIN: name might exist with the other
		// address family (e.g., AAAA when we only have A).
		h.writeAddrAnswer(w, r, qname, qtype, nil)
		return
	}
	answers := make([]string, 0, len(ips))
	for _, ip := range ips {
		answers = append(answers, ip.String())
	}
	slog.Info("dns query", slog.String("qname", qname), slog.String("qtype", qtypeStr), slog.String("decision", "peer"), slog.String("answer", strings.Join(answers, ",")))
	h.writeAddrAnswer(w, r, qname, qtype, ips)
}

func (h *dnsHandler) writeAddrAnswer(w dns.ResponseWriter, r *dns.Msg, qname string, qtype uint16, ips []net.IP) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	for _, ip := range ips {
		isV4 := ip.To4() != nil
		switch qtype {
		case dns.TypeA:
			if !isV4 {
				continue
			}
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   ip.To4(),
			})
		case dns.TypeAAAA:
			if isV4 {
				continue
			}
			m.Answer = append(m.Answer, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60},
				AAAA: ip,
			})
		}
	}
	_ = w.WriteMsg(m)
}

// peerLookup answers from the tsnet node's own peer list, populated by the
// tailnet control plane when this tsnet.Server signed in. No system
// tailscaled required: tsnet is itself a Tailscale node and knows every
// peer's DNSName and tailnet IPs.
func (h *dnsHandler) peerLookup(qname string, qtype uint16) ([]net.IP, error) {
	lc, err := h.ts.LocalClient()
	if err != nil {
		return nil, err
	}
	st, err := lc.Status(h.ctx)
	if err != nil {
		return nil, err
	}
	target := strings.ToLower(dns.Fqdn(qname))
	for _, p := range st.Peer {
		if strings.ToLower(p.DNSName) != target {
			continue
		}
		var ips []net.IP
		for _, a := range p.TailscaleIPs {
			switch {
			case qtype == dns.TypeA && a.Is4():
				ips = append(ips, net.IP(a.AsSlice()))
			case qtype == dns.TypeAAAA && a.Is6():
				ips = append(ips, net.IP(a.AsSlice()))
			}
		}
		return ips, nil
	}
	return nil, nil
}
