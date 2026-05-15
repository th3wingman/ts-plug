package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"tailscale.com/tsnet"
)

func serveTCP(ctx context.Context, ts *tsnet.Server, ln net.Listener, upstream, selfIP string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			slog.Error("accept failed", slog.String("listen", ln.Addr().String()), slog.Any("error", err))
			return
		}
		slog.Info("tcp accepted", slog.String("listen", ln.Addr().String()), slog.String("remote", c.RemoteAddr().String()), slog.String("upstream", upstream))
		go pipe(ctx, ts, c, upstream, "tcp", selfIP)
	}
}

func serveSNI(ctx context.Context, ts *tsnet.Server, ln net.Listener, routes map[string]string, selfIP string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			slog.Error("accept failed", slog.String("listen", ln.Addr().String()), slog.Any("error", err))
			return
		}
		go func(c net.Conn) {
			sni, replayed, err := peekSNI(c)
			if err != nil {
				slog.Warn("sni peek failed", slog.String("remote", c.RemoteAddr().String()), slog.Any("error", err))
				c.Close()
				return
			}
			upstream, ok := routes[sni]
			if !ok {
				slog.Warn("no route for sni", slog.String("sni", sni), slog.String("remote", c.RemoteAddr().String()))
				c.Close()
				return
			}
			slog.Info("sni accepted", slog.String("listen", ln.Addr().String()), slog.String("remote", c.RemoteAddr().String()), slog.String("sni", sni), slog.String("upstream", upstream))
			pipe(ctx, ts, replayed, upstream, "sni:"+sni, selfIP)
		}(c)
	}
}

// pipe forwards bytes between client and upstream. Before dialing, the
// upstream hostname is resolved via the tsnet identity (peer list first,
// then tsnet's internal MagicDNS) — never the system resolver, which our
// DNS responder may have hijacked. If the resolved address is our own
// local_ip, the dial is refused to break what would otherwise be a self-
// recursion loop.
func pipe(ctx context.Context, ts *tsnet.Server, client net.Conn, upstream, tag, selfIP string) {
	defer client.Close()

	target, via, err := resolveUpstream(ctx, ts, upstream)
	if err != nil {
		slog.Error("resolve upstream failed", slog.String("tag", tag), slog.String("upstream", upstream), slog.Any("error", err))
		return
	}
	slog.Info("resolved upstream", slog.String("tag", tag), slog.String("name", upstream), slog.String("target", target), slog.String("via", via))

	if selfIP != "" {
		host, _, _ := net.SplitHostPort(target)
		if host == selfIP {
			slog.Error("refusing self-dial; upstream resolves to local_ip — likely a DNS feedback loop. Put a real IP in the upstream field or fix resolution.",
				slog.String("tag", tag), slog.String("upstream", upstream), slog.String("target", target))
			return
		}
	}

	start := time.Now()
	up, err := ts.Dial(ctx, "tcp", target)
	if err != nil {
		slog.Error("dial upstream failed", slog.String("tag", tag), slog.String("upstream", target), slog.Any("error", err))
		return
	}
	slog.Info("upstream connected", slog.String("tag", tag), slog.String("upstream", target), slog.Duration("dial_took", time.Since(start)))
	defer up.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(up, client); done <- struct{}{} }()
	go func() { io.Copy(client, up); done <- struct{}{} }()
	<-done
	slog.Info("upstream closed", slog.String("tag", tag), slog.String("upstream", target), slog.Duration("connection_age", time.Since(start)))
}

// resolveUpstream turns a "host:port" into "ip:port" using tsnet-internal
// sources only: peer list first (fastest, works for real peer devices),
// then LocalClient.QueryDNS (works for admin-side additional records and
// SplitDNS rules). The system resolver is never consulted, so a hijacked
// /etc/resolv.conf can't loop us back to ourselves. Returns the resolved
// "ip:port", the method used, and an error if neither path produced an
// answer.
func resolveUpstream(ctx context.Context, ts *tsnet.Server, hostport string) (string, string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", "", fmt.Errorf("split %q: %w", hostport, err)
	}
	if net.ParseIP(host) != nil {
		return hostport, "literal", nil
	}

	lc, err := ts.LocalClient()
	if err != nil {
		return "", "", fmt.Errorf("local client: %w", err)
	}

	if st, err := lc.Status(ctx); err == nil {
		target := strings.ToLower(host) + "."
		for _, p := range st.Peer {
			if strings.ToLower(p.DNSName) != target {
				continue
			}
			for _, a := range p.TailscaleIPs {
				if a.Is4() {
					return net.JoinHostPort(a.String(), port), "peer", nil
				}
			}
		}
	}

	// Fall back to tsnet's internal MagicDNS resolver, which sees
	// admin-side additional A records and SplitDNS rules that don't
	// appear in the peer list.
	raw, _, err := lc.QueryDNS(ctx, host+".", "A")
	if err != nil {
		return "", "", fmt.Errorf("magicdns query: %w", err)
	}
	for _, ip := range parseAFromDNS(raw) {
		return net.JoinHostPort(ip.String(), port), "magicdns", nil
	}
	return "", "", fmt.Errorf("no address for %s", host)
}

// parseAFromDNS extracts A-record IPs from a raw DNS response. Hand-rolled
// here so dns.go isn't imported by proxy.go.
func parseAFromDNS(raw []byte) []net.IP {
	var ips []net.IP
	// Minimal parser: skip 12-byte header, skip the single question
	// section, then walk answers. We trust the response shape since it
	// came from our own tsnet LocalAPI.
	if len(raw) < 12 {
		return nil
	}
	ancount := int(raw[6])<<8 | int(raw[7])
	i := 12
	// skip question name
	for i < len(raw) && raw[i] != 0 {
		i += int(raw[i]) + 1
	}
	i += 5 // null label + qtype + qclass
	for n := 0; n < ancount && i < len(raw); n++ {
		// skip name (possibly compressed)
		if i < len(raw) && raw[i]&0xc0 == 0xc0 {
			i += 2
		} else {
			for i < len(raw) && raw[i] != 0 {
				i += int(raw[i]) + 1
			}
			i++
		}
		if i+10 > len(raw) {
			return ips
		}
		rrtype := int(raw[i])<<8 | int(raw[i+1])
		rdlen := int(raw[i+8])<<8 | int(raw[i+9])
		i += 10
		if rrtype == 1 && rdlen == 4 && i+4 <= len(raw) {
			ips = append(ips, net.IP(raw[i:i+4]).To4())
		}
		i += rdlen
	}
	return ips
}

// peekSNI reads the TLS ClientHello from c without disturbing the byte
// stream, returning the SNI ServerName and a net.Conn that replays the
// ClientHello to the next reader before continuing with c's live bytes.
//
// Implementation: tee c into a buffer, run a tls.Server handshake with a
// GetConfigForClient hook that captures ServerName and aborts. The hook
// fires before the server writes anything, but Write is also stubbed out
// for safety so the peek is strictly read-only on the wire.
func peekSNI(c net.Conn) (string, net.Conn, error) {
	var buf bytes.Buffer
	tee := &teeConn{Conn: c, r: io.TeeReader(c, &buf)}
	var sni string
	stop := errors.New("sni-captured")
	cfg := &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = h.ServerName
			return nil, stop
		},
	}
	_ = tls.Server(tee, cfg).Handshake()
	if sni == "" {
		return "", nil, errors.New("no SNI in ClientHello")
	}
	return sni, &replayConn{Conn: c, r: io.MultiReader(&buf, c)}, nil
}

type teeConn struct {
	net.Conn
	r io.Reader
}

func (t *teeConn) Read(b []byte) (int, error)  { return t.r.Read(b) }
func (t *teeConn) Write(b []byte) (int, error) { return len(b), nil }

type replayConn struct {
	net.Conn
	r io.Reader
}

func (r *replayConn) Read(b []byte) (int, error) { return r.r.Read(b) }
