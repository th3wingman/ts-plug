package main

import (
	"context"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
)

// The datapath probe must target only peers control reports online (an offline
// peer must not read as our datapath being dead), cap the tries at two, and
// pick the same pair each tick (lowest IPs first).
func TestProbeTargets(t *testing.T) {
	got := probeTargets([]*ipnstate.PeerStatus{
		{Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.9")}},
		{Online: false, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")}}, // offline: excluded even though lowest
		{Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.3")}},
		{Online: true, TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.5")}},
		nil,
	})
	if len(got) != 2 {
		t.Fatalf("got %d targets (%v), want 2", len(got), got)
	}
	if got[0] != netip.MustParseAddr("100.64.0.3") || got[1] != netip.MustParseAddr("100.64.0.5") {
		t.Errorf("targets = %v, want the two lowest online IPs 100.64.0.3, 100.64.0.5", got)
	}
	if again := probeTargets(nil); again != nil {
		t.Errorf("nil peers: got %v, want nil", again)
	}
}

// The self-heal decision must not restart a node that never connected (still
// logging in), must restart one that was online and has gone dark, and must
// respect the cooldown so a long outage cannot thrash it.
func TestNeedsRestart(t *testing.T) {
	now := time.Now()

	if needsRestart(&tailnetHealth{}, false, now) {
		t.Error("restart before the node was ever online")
	}
	h := &tailnetHealth{everOnline: true, lastOnline: now.Add(-10 * time.Second)}
	if needsRestart(h, false, now) {
		t.Error("restart while healthy")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now.Add(-3 * unhealthyAfter)}
	if !needsRestart(h, false, now) {
		t.Error("no restart when stuck offline past the grace window")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now}
	if !needsRestart(h, true, now) {
		t.Error("no restart for a dead forwarder")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now.Add(-time.Hour), lastRestart: now.Add(-time.Minute)}
	if needsRestart(h, true, now) {
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
