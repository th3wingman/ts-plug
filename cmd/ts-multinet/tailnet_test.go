package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Close must not return until the tailnet's goroutines (forwarder, watcher)
// have fully unwound: the TUN interface lives until every fd reference drops,
// so closing early makes a stop/start of the same tun name fail with
// TUNSETIFF EBUSY. Close cancels the ctx itself, then waits.
func TestCloseWaitsForGoroutines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tn := &Tailnet{cancel: cancel, wg: &sync.WaitGroup{}}

	var running, exited atomic.Int32
	tn.wg.Add(2)
	for range 2 {
		go func() {
			defer tn.wg.Done()
			running.Add(1)
			<-ctx.Done()
			time.Sleep(60 * time.Millisecond) // unwinding takes a moment (fd release, pumps draining)
			exited.Add(1)
		}()
	}
	for running.Load() != 2 {
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	tn.Close() // nil tun/ts: only the cancel+wait path runs
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("Close returned after %v — it did not wait for the goroutines to unwind", elapsed)
	}
	if exited.Load() != 2 {
		t.Fatal("Close returned before both goroutines exited")
	}
}
