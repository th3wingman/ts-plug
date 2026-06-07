// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// isICMPv4Echo reports whether raw IP bytes are an ICMPv4 echo request.
func isICMPv4Echo(pkt []byte) bool {
	ip := header.IPv4(pkt)
	if !ip.IsValid(len(pkt)) || ip.Protocol() != uint8(header.ICMPv4ProtocolNumber) {
		return false
	}
	p := ip.Payload()
	if len(p) < header.ICMPv4MinimumSize {
		return false
	}
	return header.ICMPv4(p).Type() == header.ICMPv4Echo
}

// handleICMPv4Echo proxies a ping: map the synthetic dst to the real tailnet
// host, probe it over the tailnet, and only then synthesize an echo reply. No
// reply means the real host is unreachable — an honest ping.
func (f *forwarder) handleICMPv4Echo(pkt []byte) {
	ip := header.IPv4(pkt)
	da := ip.DestinationAddress()
	dst := net.IP(da.AsSlice())

	host, ok := f.reg.lookup(dst)
	if !ok {
		slog.Debug("icmp: no mapping for dst", "tailnet", f.name, "dst", dst)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ipStr, ok := f.resolve(ctx, host)
	if !ok {
		slog.Debug("icmp: resolve failed", "tailnet", f.name, "host", host)
		return
	}
	realIP, err := netip.ParseAddr(ipStr)
	if err != nil || f.reg.isSynthetic(net.ParseIP(ipStr)) {
		return
	}
	rtt, ok := f.ping(ctx, realIP)
	if !ok {
		slog.Debug("icmp: ping unreachable", "tailnet", f.name, "host", host, "ip", ipStr)
		return // unreachable — drop, so ping reports loss honestly
	}
	slog.Debug("icmp: echo reply", "tailnet", f.name, "host", host, "ip", ipStr, "rtt", rtt)
	if reply := buildEchoReply(pkt); reply != nil {
		if _, err := f.tun.Write(reply); err != nil && ctx.Err() == nil {
			slog.Warn("icmp: tun write", "tailnet", f.name, "err", err)
		}
	}
}

// buildEchoReply turns an echo request into a reply in place: swap src/dst, flip
// the ICMP type, and recompute both checksums (IP header + ICMP message).
func buildEchoReply(req []byte) []byte {
	reply := append([]byte(nil), req...)
	ip := header.IPv4(reply)

	src := ip.SourceAddress()
	ip.SetSourceAddress(ip.DestinationAddress())
	ip.SetDestinationAddress(src)
	ip.SetChecksum(0)
	ip.SetChecksum(^ip.CalculateChecksum())

	icmp := header.ICMPv4(ip.Payload())
	icmp.SetType(header.ICMPv4EchoReply)
	icmp.SetChecksum(0)
	// icmp already holds the full message (header+payload), and ICMPv4Checksum
	// sums h[4:] — which covers the payload — so payloadCsum must be 0 here.
	icmp.SetChecksum(header.ICMPv4Checksum(icmp, 0))
	return reply
}
