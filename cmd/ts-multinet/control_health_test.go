package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// The self-heal decision must not restart a node that never connected (still
// logging in), must restart one that was online and has gone dark, and must
// respect the cooldown so a long outage cannot thrash it.
func TestNeedsRestart(t *testing.T) {
	now := time.Now()

	if needsRestart(&tailnetHealth{}, false, false, now) {
		t.Error("restart before the node was ever online")
	}
	h := &tailnetHealth{everOnline: true, lastOnline: now.Add(-10 * time.Second)}
	if needsRestart(h, true, false, now) {
		t.Error("restart while healthy")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now.Add(-3 * unhealthyAfter)}
	if !needsRestart(h, false, false, now) {
		t.Error("no restart when stuck offline past the grace window")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now}
	if !needsRestart(h, true, true, now) {
		t.Error("no restart for a dead forwarder")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now.Add(-time.Hour), lastRestart: now.Add(-time.Minute)}
	if needsRestart(h, false, true, now) {
		t.Error("restart during the cooldown window")
	}
}

// /restart must stop and start the tailnet in place (state kept).
func TestRestartTailnetEndpoint(t *testing.T) {
	d, _ := stubDaemon(t)

	var starts int
	restarted := make(chan struct{})
	d.rt.ctx = context.Background()
	d.rt.cleanupTUN = func(cidr, dev string) {}
	d.rt.starter = func(ctx context.Context, tc TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
		starts++
		close(restarted)
		return &Tailnet{conf: tc, wg: &sync.WaitGroup{}}, nil
	}

	rec := doReq(t, d, "POST", "/tailnet/dev/restart", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("restart = %d %s", rec.Code, rec.Body.String())
	}
	<-restarted
	if starts != 1 {
		t.Fatalf("starter called %d times, want 1", starts)
	}
	if d.tailnetByName("dev") == nil {
		t.Fatal("dev not running after restart")
	}

	// a parked tailnet is not restartable — reload is the right call
	d.tailnets = nil
	rec = doReq(t, d, "POST", "/tailnet/dev/restart", "{}")
	if rec.Code != http.StatusConflict {
		t.Fatalf("restart of parked tailnet = %d %s, want 409", rec.Code, rec.Body.String())
	}
}
