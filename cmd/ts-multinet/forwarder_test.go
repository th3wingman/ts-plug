package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// A pollable file models the idle TUN read. Shutdown must close it and join
// both pumps, not leave a blocked reader holding the interface reference.
func TestForwarderShutdownReleasesReader(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newForwarder("test", r, 1280, nil, nil, nil, nil)
	result := make(chan error, 1)
	go func() { result <- f.run(ctx) }()
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown left the TUN reader blocked")
	}
	if _, err := r.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("TUN file remains open: %v", err)
	}
}
