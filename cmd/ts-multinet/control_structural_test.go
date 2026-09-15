package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The structural endpoints change cidr/tun in the config (comments survive)
// and let the sync path stop/start the tailnet. syncDaemon stubs the starter
// (no tsnet/TUN) and the tun cleanup, so these stay hermetic.
func TestStructuralCIDRAndTUN(t *testing.T) {
	d, cfgPath, stub := syncDaemon(t)

	rec := doReq(t, d, "POST", "/tailnet/dev/cidr", `{"cidr":"198.18.5.0/24"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cidr set: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).CIDR; got != "198.18.5.0/24" {
		t.Fatalf("cidr in config = %q", got)
	}
	select { // the change restarts the tailnet (stub records the teardown)
	case stopped := <-stub.stopped:
		if stopped != "dev" {
			t.Fatalf("stopped %q, want dev", stopped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cidr change did not restart the tailnet")
	}

	// rejections never write
	for _, c := range []struct{ body, want string }{
		{`{"cidr":""}`, "cidr is required"},
		{`{"cidr":"10.0.0.0/24"}`, "outside the synthetic range"},
		{`{"cidr":"nonsense"}`, "invalid IPv4 cidr"},
	} {
		rec := doReq(t, d, "POST", "/tailnet/dev/cidr", c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("cidr %s: %d %s, want 400 %q", c.body, rec.Code, rec.Body.String(), c.want)
		}
	}
	if got := devConf(t, cfgPath).CIDR; got != "198.18.5.0/24" {
		t.Fatalf("rejected cidr must not write, got %q", got)
	}
	// same value is an idempotent no-op
	if rec := doReq(t, d, "POST", "/tailnet/dev/cidr", `{"cidr":"198.18.5.0/24"}`); rec.Code != http.StatusOK {
		t.Fatalf("cidr no-op: %d %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, d, "POST", "/tailnet/dev/tun", `{"tun":"tsm9"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tun set: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).TUN; got != "tsm9" {
		t.Fatalf("tun in config = %q", got)
	}
	for _, c := range []struct{ body, want string }{
		{`{"tun":""}`, "tun is required"},
		{`{"tun":"way-too-long-device-name"}`, "1-15 chars"},
	} {
		rec := doReq(t, d, "POST", "/tailnet/dev/tun", c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("tun %s: %d %s, want 400 %q", c.body, rec.Code, rec.Body.String(), c.want)
		}
	}

	// unknown tailnet
	if rec := doReq(t, d, "POST", "/tailnet/ghost/cidr", `{"cidr":"198.18.7.0/24"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown tailnet: %d %s", rec.Code, rec.Body.String())
	}
}

// Overlap and device clashes are checked against every other tailnet, and the
// edited tailnet's own values never count as a clash with themselves.
func TestStructuralClashChecks(t *testing.T) {
	d, cfgPath, _ := syncDaemon(t)

	if rec := doReq(t, d, "POST", "/tailnet", `{"name":"corp","cidr":"198.18.3.0/24","tun":"tsm3"}`); rec.Code != http.StatusOK {
		t.Fatalf("add corp: %d %s", rec.Code, rec.Body.String())
	}

	rec := doReq(t, d, "POST", "/tailnet/dev/cidr", `{"cidr":"198.18.3.0/24"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "overlaps tailnet") {
		t.Fatalf("cidr overlap: %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, d, "POST", "/tailnet/dev/cidr", `{"cidr":"198.18.2.0/23"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "overlaps tailnet") {
		t.Fatalf("cidr superset overlap: %d %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, d, "POST", "/tailnet/dev/tun", `{"tun":"tsm3"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already used by tailnet") {
		t.Fatalf("tun clash: %d %s", rec.Code, rec.Body.String())
	}

	// editing a tailnet to its own current values stays a no-op, not a clash
	if rec := doReq(t, d, "POST", "/tailnet/dev/tun", `{"tun":"tsm0"}`); rec.Code != http.StatusOK {
		t.Fatalf("tun self no-op: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/cidr", `{"cidr":"198.18.1.0/24"}`); rec.Code != http.StatusOK {
		t.Fatalf("cidr self no-op: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).CIDR; got != "198.18.1.0/24" {
		t.Fatalf("dev cidr drifted: %q", got)
	}
}

// A parked tailnet (in config, not running) accepts structural edits too.
func TestStructuralOnParkedTailnet(t *testing.T) {
	d, cfgPath, _ := syncDaemon(t)
	dir := filepath.Dir(cfgPath)
	parked := `{"state_dir": "` + dir + `", "tailnets": [` +
		`{"name":"dev","cidr":"198.18.1.0/24","tun":"tsm0"},` +
		`{"name":"prod","cidr":"198.18.2.0/24","tun":"tsm1"}]}`
	if err := os.WriteFile(cfgPath, []byte(parked), 0600); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, d, "POST", "/tailnet/prod/cidr", `{"cidr":"198.18.8.0/24"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("parked cidr set: %d %s", rec.Code, rec.Body.String())
	}
	if got := confByName(mustLoad(t, cfgPath), "prod"); got == nil || got.CIDR != "198.18.8.0/24" {
		t.Fatalf("parked prod cidr not written: %+v", got)
	}
}
