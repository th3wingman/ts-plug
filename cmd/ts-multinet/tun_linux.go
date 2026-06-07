// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openTUN creates a TUN device and returns its file plus the kernel-assigned
// name. IFF_NO_PI means each read/write is a bare IP packet (no 4-byte prefix),
// which is exactly what gVisor's link endpoint wants.
func openTUN(name string) (*os.File, string, error) {
	fd, err := unix.Open("/dev/net/tun", os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var ifr struct {
		name  [unix.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	copy(ifr.name[:], name)
	ifr.flags = unix.IFF_TUN | unix.IFF_NO_PI
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		unix.Close(fd)
		return nil, "", fmt.Errorf("TUNSETIFF %q: %w", name, errno)
	}
	dev := unix.ByteSliceToString(ifr.name[:])
	return os.NewFile(uintptr(fd), "/dev/net/tun"), dev, nil
}

// bringUp sets the device MTU and brings it up. No address is assigned: the
// kernel routes the synthetic CIDR to the device by route alone.
func bringUp(dev string, mtu uint32) error {
	return run("ip", "link", "set", "dev", dev, "mtu", strconv.Itoa(int(mtu)), "up")
}

// addRoute points a synthetic CIDR at the device. Plain route, no policy
// routing — the whole point of disjoint synthetic ranges.
func addRoute(cidr, dev string) error {
	return run("ip", "route", "replace", cidr, "dev", dev)
}

// addAddr puts an address on the device so the kernel sources synthetic-range
// traffic from it (our real tailnet IP) instead of bouncing off eth0.
func addAddr(dev, ipCIDR string) error {
	return run("ip", "addr", "add", ipCIDR, "dev", dev)
}

func run(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %w: %s", args, err, out)
	}
	return nil
}
