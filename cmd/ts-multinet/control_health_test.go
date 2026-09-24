package main

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
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

// A live tailnet stuck in NeedsLogin restarts when a key arrives, so tsnet
// (which reads AuthKey at start) enrolls without a browser. An empty key is
// rejected outright — a no-op set would look like a success.
func TestAuthKeyRestartsNeedsLogin(t *testing.T) {
	d, _ := stubDaemon(t)
	var restarted bool
	d.rt.ctx = context.Background()
	d.rt.cleanupTUN = func(cidr, dev string) {}
	d.rt.starter = func(ctx context.Context, tc TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
		restarted = true
		// state stays "": applySelections skips non-Running tailnets, and a
		// stub has no lc for it to call
		return &Tailnet{conf: tc, wg: &sync.WaitGroup{}}, nil
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/authkey", `{"auth_key":"tskey-auth-k2"}`); rec.Code != http.StatusOK {
		t.Fatalf("authkey = %d %s", rec.Code, rec.Body.String())
	}
	if !restarted {
		t.Fatal("NeedsLogin tailnet was not restarted to use the key")
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/authkey", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty key = %d %s, want 400", rec.Code, rec.Body.String())
	}
}

// The enabled toggle stops and starts a tailnet without unprovisioning: node
// state (and login) is kept, so re-enabling starts it right back up. A
// disabled tailnet stays visible in status as "disabled", not "gone".
func TestEnabledToggle(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	d.rt.ctx = context.Background()
	d.rt.cleanupTUN = func(cidr, dev string) {}
	var starts int
	d.rt.starter = func(ctx context.Context, tc TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
		starts++
		return &Tailnet{conf: tc, wg: &sync.WaitGroup{}}, nil
	}

	if rec := doReq(t, d, "POST", "/tailnet/dev/enabled", `{"on":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if d.tailnetByName("dev") != nil {
		t.Fatal("disabled tailnet still live")
	}
	if tc := devConf(t, cfgPath); tc.Enabled == nil || *tc.Enabled {
		t.Fatal("enabled=false not persisted")
	}
	rec := doReq(t, d, "GET", "/status", "")
	if !strings.Contains(rec.Body.String(), `"state":"disabled"`) {
		t.Fatalf("status missing the disabled entry: %s", rec.Body.String())
	}

	if rec := doReq(t, d, "POST", "/tailnet/dev/enabled", `{"on":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d %s", rec.Code, rec.Body.String())
	}
	if starts != 1 {
		t.Fatalf("re-enable started the node %d times, want 1", starts)
	}
	if d.tailnetByName("dev") == nil {
		t.Fatal("re-enabled tailnet not live")
	}
}

// The self-heal decision must not restart a node that never connected (still
// logging in), must restart one that was online and has gone dark, and must
// respect the cooldown so a long outage cannot thrash it. A forced pass (the
// network just changed) drops the 90s grace: a once-online node still dark
// right after the link settled is restarted immediately.
func TestNeedsRestart(t *testing.T) {
	now := time.Now()

	if needsRestart(&tailnetHealth{}, false, now, false) {
		t.Error("restart before the node was ever online")
	}
	h := &tailnetHealth{everOnline: true, lastOnline: now.Add(-10 * time.Second)}
	if needsRestart(h, false, now, false) {
		t.Error("restart while healthy")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now.Add(-3 * unhealthyAfter)}
	if !needsRestart(h, false, now, false) {
		t.Error("no restart when stuck offline past the grace window")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now}
	if !needsRestart(h, true, now, false) {
		t.Error("no restart for a dead forwarder")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now.Add(-time.Hour), lastRestart: now.Add(-time.Minute)}
	if needsRestart(h, true, now, false) {
		t.Error("restart during the cooldown window")
	}

	// forced: recent lastOnline is no longer protection, but never-online
	// still is, and so is the cooldown
	h = &tailnetHealth{everOnline: true, lastOnline: now.Add(-5 * time.Second)}
	if !needsRestart(h, false, now, true) {
		t.Error("no forced restart for a once-online node dark right after a network change")
	}
	if needsRestart(&tailnetHealth{}, false, now, true) {
		t.Error("forced restart of a node that was never online")
	}
	h = &tailnetHealth{everOnline: true, lastOnline: now, lastRestart: now.Add(-time.Minute)}
	if needsRestart(h, false, now, true) {
		t.Error("forced restart during the cooldown window")
	}
}

// The suspend detector: kernel uptime (counts suspend) minus monotonic
// elapsed (doesn't) is the time spent asleep; jitter must not read as a
// suspend in either direction.
func TestSuspendedSince(t *testing.T) {
	cases := []struct {
		uptime, monotonic, want time.Duration
	}{
		{uptime: 60 * time.Second, monotonic: 60 * time.Second, want: 0},               // awake
		{uptime: 90 * time.Second, monotonic: 3 * time.Second, want: 87 * time.Second}, // resumed 87s into the sleep
		{uptime: 59 * time.Second, monotonic: 60 * time.Second, want: 0},               // uptime read a hair early
	}
	for _, c := range cases {
		if got := suspendedSince(c.uptime, c.monotonic); got != c.want {
			t.Errorf("suspendedSince(%v, %v) = %v, want %v", c.uptime, c.monotonic, got, c.want)
		}
	}
}

// The resume detector's /proc/uptime parser: first field is seconds of
// CLOCK_BOOTTIME (suspend included); garbage is an error, not a panic.
func TestParseUptime(t *testing.T) {
	up, err := parseUptime("123456.78 234567.89\n")
	if err != nil || up != 123456780*time.Millisecond {
		t.Fatalf("parseUptime = %v, %v; want 123456.78s", up, err)
	}
	if _, err := parseUptime(""); err == nil {
		t.Error("empty /proc/uptime parsed without error")
	}
	if _, err := parseUptime("not-a-number 5\n"); err == nil {
		t.Error("garbage uptime parsed without error")
	}
}

// The route-change detector: default routes (destination 00000000) as
// comparable text — the header and non-default rows are skipped, rows are
// sorted so a reorder is not a change, and no default route is "".
func TestParseDefaultRoutes(t *testing.T) {
	proc := `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	RTERF
wlp0s20f3	00000000	0100A8C0	0003	0	0	600	00000000	0	0	0
wlp0s20f3	0069A8C0	0100A8C0	0003	0	0	600	00FFFFFF	0	0	0
tsm0	00000000	00000000	0001	0	0	0	00000000	0	0	0
`
	got := parseDefaultRoutes(proc)
	want := "tsm0 00000000\nwlp0s20f3 0100A8C0" // sorted; the 192.168.105.0 row is not default
	if got != want {
		t.Errorf("parseDefaultRoutes = %q, want %q", got, want)
	}
	if again := parseDefaultRoutes(proc); again != got {
		t.Errorf("parseDefaultRoutes not stable: %q then %q", got, again)
	}
	if got := parseDefaultRoutes("Iface\tDestination\tGateway\n"); got != "" {
		t.Errorf("no default route = %q, want empty", got)
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
