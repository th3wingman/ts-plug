package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// testNet wires a forwarder to a client gVisor stack over a SOCK_SEQPACKET
// socketpair — a fake TUN that keeps packet boundaries both ways. The client
// plays the kernel/app side; the forwarder dials through the injected dial,
// which tests point at local listeners (or make fail).
type testNet struct {
	client *stack.Stack
	peer   tcpip.Address // synthetic IP allocated to peer.test.ts.net
	reg    *registry
}

func newTestNet(t *testing.T, mtu uint32, dial dialFunc) *testNet {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil { // pollable, so Close unblocks Read
			t.Fatal(err)
		}
	}
	fwdFile, cliFile := os.NewFile(uintptr(fds[0]), "fwd"), os.NewFile(uintptr(fds[1]), "cli")

	reg, err := newRegistry([]TailnetConf{{Name: "test", CIDR: "198.18.1.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	ip, ok := reg.allocate("test", "peer.test.ts.net")
	if !ok {
		t.Fatal("allocate failed")
	}
	resolve := func(context.Context, string) (string, bool) { return "127.0.0.1", true }

	ctx, cancel := context.WithCancel(context.Background())
	f := newForwarder("test", fwdFile, mtu, reg, dial, resolve, nil)
	fwdDone := make(chan error, 1)
	go func() { fwdDone <- f.run(ctx) }()

	cs := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	cep := channel.New(512, mtu, "")
	if err := cs.CreateNIC(nicID, cep); err != nil {
		t.Fatalf("client CreateNIC: %v", err)
	}
	if err := cs.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4([4]byte{100, 64, 0, 1}).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("client address: %v", err)
	}
	cs.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})

	cctx, ccancel := context.WithCancel(context.Background())
	go func() { // fake TUN → client stack
		buf := make([]byte, int(mtu)+128)
		for {
			n, err := cliFile.Read(buf)
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(buf[:n])})
			cep.InjectInbound(ipv4.ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
	go func() { // client stack → fake TUN
		for {
			pkt := cep.ReadContext(cctx)
			if pkt == nil {
				return
			}
			_, _ = cliFile.Write(pkt.ToView().AsSlice())
			pkt.DecRef()
		}
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-fwdDone:
		case <-time.After(3 * time.Second):
			t.Error("forwarder did not shut down")
		}
		ccancel()
		cliFile.Close()
		cep.Close()
		cs.Close()
	})
	return &testNet{client: cs, peer: tcpip.AddrFrom4(netip.MustParseAddr(ip.String()).As4()), reg: reg}
}

func (n *testNet) dialTCP(ctx context.Context, addr tcpip.Address, port uint16) (*gonet.TCPConn, error) {
	return gonet.DialContextTCP(ctx, n.client, tcpip.FullAddress{NIC: nicID, Addr: addr, Port: port}, ipv4.ProtocolNumber)
}

// localDial is the tailnet egress in tests: a plain dial to 127.0.0.1.
func localDial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// tcpEchoServer listens on 127.0.0.1 and echoes every connection back.
func tcpEchoServer(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

func udpEchoServer(t *testing.T) uint16 {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	return uint16(pc.LocalAddr().(*net.UDPAddr).Port)
}

// tcpEcho pushes size random bytes through the forwarder and checks they come
// back intact.
func tcpEcho(t *testing.T, mtu uint32, size int) {
	t.Helper()
	port := tcpEchoServer(t)
	n := newTestNet(t, mtu, localDial)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := n.dialTCP(ctx, n.peer, port)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	want := make([]byte, size)
	rand.Read(want)
	go func() { _, _ = c.Write(want) }()
	got := make([]byte, size)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("echoed bytes differ")
	}
}

func udpEcho(t *testing.T, mtu uint32, size int) {
	t.Helper()
	port := udpEchoServer(t)
	n := newTestNet(t, mtu, localDial)
	c, err := gonet.DialUDP(n.client, nil, &tcpip.FullAddress{NIC: nicID, Addr: n.peer, Port: port}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("udp dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	want := make([]byte, size)
	rand.Read(want)
	if _, err := c.Write(want); err != nil {
		t.Fatalf("udp write: %v", err)
	}
	got := make([]byte, 64*1024)
	m, err := c.Read(got)
	if err != nil {
		t.Fatalf("udp read: %v", err)
	}
	if !bytes.Equal(got[:m], want) {
		t.Fatalf("udp echo differs: got %d bytes, want %d", m, size)
	}
}

func TestForwarderTCPEcho(t *testing.T) { tcpEcho(t, 1280, 1<<20) }

func TestForwarderUDPEcho(t *testing.T) { udpEcho(t, 1280, 1200) }

// A synthetic IP nobody was allocated has no name to dial: reset, not a hang.
func TestForwarderUnknownSyntheticRST(t *testing.T) {
	n := newTestNet(t, 1280, localDial)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := n.dialTCP(ctx, tcpip.AddrFrom4([4]byte{198, 18, 1, 99}), 80)
	if err == nil {
		c.Close()
		t.Fatal("connect to unallocated synthetic IP succeeded")
	}
	if ctx.Err() != nil {
		t.Fatalf("connect hung until timeout instead of being reset: %v", err)
	}
}

// A large local MTU carries big segments and datagrams end to end: the TUN
// hop is independent of the tailnet MTU.
func TestForwarderLargeMTU(t *testing.T) {
	t.Run("tcp", func(t *testing.T) { tcpEcho(t, 65520, 1<<20) })
	t.Run("udp", func(t *testing.T) { udpEcho(t, 65520, 8000) })
}
