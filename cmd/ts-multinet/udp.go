// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// udpIdle reaps a UDP flow after this long with no traffic in either
// direction. UDP has no close, so without this we'd leak endpoints forever.
const udpIdle = 60 * time.Second

// setupUDP installs the UDP forwarder. It fires once per 4-tuple (subsequent
// datagrams for an established flow bypass it), so each call starts one relay.
func (f *forwarder) setupUDP() {
	fwd := udp.NewForwarder(f.stack, func(req *udp.ForwarderRequest) {
		id := req.ID()
		host, ok := f.reg.lookup(net.IP(id.LocalAddress.AsSlice()))
		if !ok {
			return // unknown synthetic dst; drop (no RST for UDP)
		}
		var wq waiter.Queue
		ep, err := req.CreateEndpoint(&wq)
		if err != nil {
			return
		}
		go f.relayUDP(gonet.NewUDPConn(&wq, ep), host, id.LocalPort)
	})
	f.stack.SetTransportProtocolHandler(udp.ProtocolNumber, fwd.HandlePacket)
}

// relayUDP resolves the destination to a real tailnet IP, dials it over the
// tailnet, and shuttles datagrams both ways until idle.
func (f *forwarder) relayUDP(local net.Conn, host string, port uint16) {
	defer local.Close()

	rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	ip, ok := f.resolve(rctx, host)
	cancel()
	if !ok {
		slog.Warn("udp: no tailnet IP for host", "tailnet", f.name, "host", host)
		return
	}
	if f.reg.isSynthetic(net.ParseIP(ip)) {
		slog.Error("udp: resolved to synthetic IP; refusing to loop", "tailnet", f.name, "host", host, "ip", ip)
		return
	}

	target := net.JoinHostPort(ip, strconv.Itoa(int(port)))
	remote, err := f.dial(context.Background(), "udp", target)
	if err != nil {
		slog.Warn("udp: tailnet dial failed", "tailnet", f.name, "host", host, "target", target, "err", err)
		return
	}
	defer remote.Close()

	slog.Info("proxying udp", "tailnet", f.name, "host", host, "via", target)
	done := make(chan struct{}, 2)
	go udpCopy(remote, local, done)
	go udpCopy(local, remote, done)
	<-done // first side to go idle/error tears down both via the defers
}

// udpCopy forwards datagrams from src to dst, resetting an idle deadline on
// each one. A read timeout (idle) or any error ends the copy.
func udpCopy(dst, src net.Conn, done chan<- struct{}) {
	buf := make([]byte, 64*1024)
	for {
		src.SetReadDeadline(time.Now().Add(udpIdle))
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	done <- struct{}{}
}
