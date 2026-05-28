// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const nicID = 1

// dialFunc is the tailnet egress. *tsnet.Server.Dial satisfies it.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// resolveFunc maps a tailnet FQDN to its real tailnet IP (e.g. 100.x.y.z) using
// that tailnet's own peer list — NOT the system resolver. Dialing by name would
// re-enter our hijacked DNS responder and loop straight back into this TUN.
type resolveFunc func(ctx context.Context, host string) (string, bool)

// forwarder runs a userspace TCP/IP stack on one TUN fd. Every TCP connection
// it accepts is resolved to a real tailnet IP and re-dialed out over the
// tailnet.
type forwarder struct {
	name    string
	tun     *os.File
	mtu     uint32
	reg     *registry
	dial    dialFunc
	resolve resolveFunc

	stack *stack.Stack
	ep    *channel.Endpoint
}

func newForwarder(name string, tun *os.File, mtu uint32, reg *registry, dial dialFunc, resolve resolveFunc) *forwarder {
	return &forwarder{name: name, tun: tun, mtu: mtu, reg: reg, dial: dial, resolve: resolve}
}

func (f *forwarder) run(ctx context.Context) error {
	f.stack = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	f.ep = channel.New(512, f.mtu, "")
	if err := f.stack.CreateNIC(nicID, f.ep); err != nil {
		return fmt.Errorf("CreateNIC: %v", err)
	}
	// Accept packets for any destination routed to this TUN, and let the stack
	// source replies from those (unowned) addresses.
	f.stack.SetPromiscuousMode(nicID, true)
	f.stack.SetSpoofing(nicID, true)
	f.stack.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	fwd := tcp.NewForwarder(f.stack, 0, 2048, f.handle)
	f.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)

	go f.tunToStack(ctx)
	go f.stackToTun(ctx)

	<-ctx.Done()
	f.ep.Close()
	f.stack.Close()
	return nil
}

// tunToStack reads IP packets off the TUN and injects them into the stack.
func (f *forwarder) tunToStack(ctx context.Context) {
	buf := make([]byte, int(f.mtu)+128)
	for {
		n, err := f.tun.Read(buf)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("tun read", "tailnet", f.name, "err", err)
			}
			return
		}
		if n == 0 {
			continue
		}
		var proto tcpip.NetworkProtocolNumber
		switch buf[0] >> 4 {
		case 4:
			proto = header.IPv4ProtocolNumber
		case 6:
			proto = header.IPv6ProtocolNumber
		default:
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(buf[:n]),
		})
		f.ep.InjectInbound(proto, pkt)
		pkt.DecRef()
	}
}

// stackToTun writes packets the stack emits back out the TUN.
func (f *forwarder) stackToTun(ctx context.Context) {
	for {
		pkt := f.ep.ReadContext(ctx)
		if pkt == nil { // context cancelled
			return
		}
		_, err := f.tun.Write(pkt.ToView().AsSlice())
		pkt.DecRef()
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("tun write", "tailnet", f.name, "err", err)
			}
			return
		}
	}
}

// handle terminates an inbound SYN locally and splices the connection to a
// tailnet dial of the original hostname.
func (f *forwarder) handle(req *tcp.ForwarderRequest) {
	id := req.ID()
	dstIP := net.IP(id.LocalAddress.AsSlice())
	dstPort := id.LocalPort

	host, ok := f.reg.lookup(dstIP)
	if !ok {
		slog.Warn("no name for synthetic dst", "tailnet", f.name, "ip", dstIP)
		req.Complete(true) // RST
		return
	}

	var wq waiter.Queue
	ep, tcpErr := req.CreateEndpoint(&wq)
	if tcpErr != nil {
		slog.Error("CreateEndpoint", "tailnet", f.name, "err", tcpErr)
		req.Complete(true)
		return
	}
	req.Complete(false)

	// Resolution + dial happen in the splice goroutine, off the packet path.
	go f.splice(gonet.NewTCPConn(&wq, ep), host, dstPort)
}

func (f *forwarder) splice(local net.Conn, host string, port uint16) {
	defer local.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ip, ok := f.resolve(ctx, host)
	if !ok {
		slog.Warn("no tailnet IP for host", "tailnet", f.name, "host", host)
		return
	}
	// Never dial back into a synthetic range — that's the feedback loop.
	if f.reg.isSynthetic(net.ParseIP(ip)) {
		slog.Error("resolved to synthetic IP; refusing to loop", "tailnet", f.name, "host", host, "ip", ip)
		return
	}

	target := net.JoinHostPort(ip, strconv.Itoa(int(port)))
	remote, err := f.dial(ctx, "tcp", target)
	if err != nil {
		slog.Warn("tailnet dial failed", "tailnet", f.name, "host", host, "target", target, "err", err)
		return
	}
	defer remote.Close()

	slog.Info("proxying", "tailnet", f.name, "host", host, "via", target)
	done := make(chan struct{}, 2)
	go func() { io.Copy(remote, local); done <- struct{}{} }()
	go func() { io.Copy(local, remote); done <- struct{}{} }()
	<-done
}
