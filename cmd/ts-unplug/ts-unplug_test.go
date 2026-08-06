package main

import (
	"net"
	"os"
	"testing"
)

func TestListenUnixSocket(t *testing.T) {
	path := t.TempDir() + "/test.sock"
	*flagSocket = path
	defer func() { *flagSocket = "" }()

	l, err := listen()
	if err != nil {
		t.Fatalf("listen() = %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket", path)
	}
	if perm := fi.Mode().Perm(); perm != 0o666 {
		t.Errorf("socket perms = %o, want 666", perm)
	}

	// Connectable?
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Errorf("dial: %v", err)
	} else {
		c.Close()
	}

	// Close unlinks; a rebind must work (stale-socket handling is for
	// unclean shutdowns, exercised below).
	l.Close()
	l2, err := listen()
	if err != nil {
		t.Fatalf("rebind after close: %v", err)
	}
	l2.Close()
}

func TestListenRefusesNonSocket(t *testing.T) {
	path := t.TempDir() + "/regular-file"
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	*flagSocket = path
	defer func() { *flagSocket = "" }()

	if _, err := listen(); err == nil {
		t.Fatal("listen() clobbered a regular file, want error")
	}
}

func TestListenRemovesStaleSocket(t *testing.T) {
	path := t.TempDir() + "/stale.sock"

	// Simulate an unclean shutdown: bind, then leave the file behind.
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket setup failed: %v", err)
	}

	*flagSocket = path
	defer func() { *flagSocket = "" }()

	l2, err := listen()
	if err != nil {
		t.Fatalf("listen() with stale socket = %v", err)
	}
	l2.Close()
}
