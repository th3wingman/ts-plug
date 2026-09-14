package main

import (
	"context"
	"net"
	"testing"

	"github.com/miekg/dns"
)

// recorder is a minimal dns.ResponseWriter capturing the last reply.
type recorder struct{ msg *dns.Msg }

func (r *recorder) WriteMsg(m *dns.Msg) error   { r.msg = m; return nil }
func (r *recorder) LocalAddr() net.Addr         { return nil }
func (r *recorder) RemoteAddr() net.Addr        { return nil }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) Close() error                { return nil }
func (r *recorder) TsigStatus() error           { return nil }
func (r *recorder) TsigTimersOnly(bool)         {}
func (r *recorder) Hijack()                     {}

// newDomainTestReg: one tailnet with a custom domain, suffix known, and a
// resolver that only knows my-server.
func newDomainTestReg(t *testing.T) *registry {
	t.Helper()
	reg, err := newRegistry([]TailnetConf{
		{Name: "skynet", Domain: "lan", CIDR: "198.18.1.0/24", TUN: "tsm0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg.registerSuffix("skynet", "tail523555.ts.net")
	reg.registerResolver("skynet", func(ctx context.Context, host string) (string, bool) {
		if host == "my-server.tail523555.ts.net" {
			return "100.64.0.1", true
		}
		return "", false
	})
	return reg
}

func TestMatchDomainAndNameAliases(t *testing.T) {
	reg := newDomainTestReg(t)

	tn, fqdn, ok := reg.match("my-server.lan.")
	if !ok || tn != "skynet" || fqdn != "my-server.tail523555.ts.net" {
		t.Fatalf("domain alias: %q %q %v", tn, fqdn, ok)
	}
	tn2, fqdn2, ok := reg.match("my-server.skynet.")
	if !ok || tn2 != "skynet" || fqdn2 != "my-server.tail523555.ts.net" {
		t.Fatalf("name alias: %q %q %v", tn2, fqdn2, ok)
	}
	// both spellings share one synthetic IP
	ip1, _ := reg.allocate(tn, fqdn)
	ip2, _ := reg.allocate(tn2, fqdn2)
	if !ip1.Equal(ip2) {
		t.Fatalf("aliases got different IPs: %s vs %s", ip1, ip2)
	}

	// runtime domain change lands via registerDomain
	reg.registerDomain("skynet", "corp")
	if _, _, ok := reg.match("my-server.lan."); ok {
		t.Fatal("old domain still matches after registerDomain")
	}
	if _, _, ok := reg.match("my-server.corp."); !ok {
		t.Fatal("new domain does not match after registerDomain")
	}
}

func TestServeDNSDomainForms(t *testing.T) {
	reg := newDomainTestReg(t)
	ds := &dnsServer{reg: reg, upstream: "1.1.1.1:53"}

	query := func(name string, qtype uint16) *dns.Msg {
		rec := &recorder{}
		q := new(dns.Msg)
		q.SetQuestion(name, qtype)
		ds.ServeDNS(rec, q)
		return rec.msg
	}

	// domain spelling answers with the same synthetic IP as the MagicDNS one
	for _, name := range []string{"my-server.lan.", "my-server.tail523555.ts.net."} {
		m := query(name, dns.TypeA)
		if m == nil || m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
			t.Fatalf("%s: expected one A answer, got %+v", name, m)
		}
	}
	ipA := query("my-server.lan.", dns.TypeA).Answer[0].(*dns.A).A
	ipB := query("my-server.tail523555.ts.net.", dns.TypeA).Answer[0].(*dns.A).A
	if !ipA.Equal(ipB) {
		t.Fatalf("domain and suffix spellings differ: %s vs %s", ipA, ipB)
	}

	// unknown host under a known domain: NXDOMAIN so resolvers fall through
	if m := query("ghost.lan.", dns.TypeA); m == nil || m.Rcode != dns.RcodeNameError {
		t.Fatalf("ghost.lan: expected NXDOMAIN, got %+v", m)
	}
}

func TestStartDNSBindConflict(t *testing.T) {
	// pre-bind an address so startDNS's synchronous bind fails
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind loopback udp:", err)
	}
	defer c.Close()

	if err := startDNS(c.LocalAddr().String(), &dnsServer{}); err == nil {
		t.Fatal("expected bind-conflict error from startDNS")
	}
	// a free port binds fine (the degraded-host decision is the caller's)
	if err := startDNS("127.0.0.1:0", &dnsServer{}); err != nil {
		t.Fatal(err)
	}
}

func TestResolvedSyncGating(t *testing.T) {
	// DNS listener down: everything no-ops, no resolvectl is exec'd
	s := newResolvedSync("127.0.0.1:53", false)
	if s.enabled {
		t.Fatal("sync enabled with DNS down")
	}
	s.register("tsm0", "tail523555.ts.net", "lan")
	s.revertAll()
	if len(s.registered) != 0 {
		t.Fatal("register recorded state while disabled")
	}

	// :53 renders as a bare IP for resolvectl; nonstandard ports keep it
	if got := resolvectlAddr("127.0.0.1:53"); got != "127.0.0.1" {
		t.Fatalf("resolvectlAddr(127.0.0.1:53) = %q", got)
	}
	if got := resolvectlAddr("127.0.0.1:5353"); got != "127.0.0.1:5353" {
		t.Fatalf("resolvectlAddr(127.0.0.1:5353) = %q", got)
	}
	if got := resolvectlAddr("127.0.0.1"); got != "127.0.0.1" {
		t.Fatalf("resolvectlAddr(127.0.0.1) = %q", got)
	}
}
