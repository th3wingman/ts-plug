package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tailscale.com/tailcfg"
)

// Locked selections are pinned essentials (jumpboxes, logging, metrics):
// they stay selected through clear-all, refuse forget, and block disabling
// or removing the tailnet that carries them.

func lockPeer(t *testing.T, d *Daemon, name string, on bool) {
	t.Helper()
	body := `{"peer":"` + name + `","on":` + map[bool]string{true: "true", false: "false"}[on] + `}`
	if rec := doReq(t, d, "POST", "/tailnet/dev/lock", body); rec.Code != http.StatusOK {
		t.Fatalf("lock %s (%v): %d %s", name, on, rec.Code, rec.Body.String())
	}
}

func TestLockImpliesSelectAndUnlockKeepsIt(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	stubServices(d, map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:web": {Name: "svc:web", DisplayName: "web"},
	})

	lockPeer(t, d, "my-server", true)
	tc := devConf(t, cfgPath)
	if len(tc.Resources) != 1 || tc.Resources[0] != "my-server" {
		t.Fatalf("lock must select: resources=%v", tc.Resources)
	}
	if len(tc.Locked) != 1 || tc.Locked[0] != "my-server" {
		t.Fatalf("locked=%v", tc.Locked)
	}
	// the patch must survive the hujson round trip: comments intact
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "// comments must survive daemon edits") || !strings.Contains(string(b), `"locked"`) {
		t.Errorf("patch lost comments or the locked member:\n%s", b)
	}

	// a service locks the same way
	lockPeer(t, d, "svc:web", true)
	tc = devConf(t, cfgPath)
	if len(tc.Locked) != 2 || len(tc.Resources) != 2 {
		t.Fatalf("after service lock: resources=%v locked=%v", tc.Resources, tc.Locked)
	}

	// unlocking releases the pin, the selection stays
	lockPeer(t, d, "my-server", false)
	tc = devConf(t, cfgPath)
	if len(tc.Locked) != 1 || tc.Locked[0] != "svc:web" {
		t.Fatalf("locked after unlock: %v", tc.Locked)
	}
	if len(tc.Resources) != 2 {
		t.Fatalf("unlock must keep the selection: %v", tc.Resources)
	}

	// locking validates like select: unknown names are refused, nothing written
	for _, c := range []struct{ name, body, want string }{
		{"unknown peer", `{"peer":"nope","on":true}`, "unknown peer"},
		{"unknown service", `{"peer":"svc:ghost","on":true}`, "unknown service"},
		{"no peer", `{"on":true}`, "peer is required"},
	} {
		rec := doReq(t, d, "POST", "/tailnet/dev/lock", c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want 400 %q", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	if got := devConf(t, cfgPath); len(got.Resources) != 2 || len(got.Locked) != 1 {
		t.Fatalf("rejected lock wrote to config: resources=%v locked=%v", got.Resources, got.Locked)
	}
}

func TestLockedSurvivesClearAll(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	if rec := doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"my-server"}`); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"db"}`); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	lockPeer(t, d, "my-server", true)

	rec := doReq(t, d, "POST", "/tailnet/dev/clear", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "kept 1 locked (my-server)") {
		t.Errorf("clear must say what it kept: %s", rec.Body.String())
	}
	tc := devConf(t, cfgPath)
	if len(tc.Resources) != 1 || tc.Resources[0] != "my-server" || tc.AllowAll {
		t.Fatalf("clear must keep locked essentials: resources=%v allow_all=%v", tc.Resources, tc.AllowAll)
	}
	if len(tc.Locked) != 1 || tc.Locked[0] != "my-server" {
		t.Fatalf("clear must not touch the locked set: %v", tc.Locked)
	}

	// select-all keeps them too: it is a union, not a replacement
	if rec := doReq(t, d, "POST", "/tailnet/dev/select-all", `{"resources":["db"]}`); rec.Code != http.StatusOK {
		t.Fatalf("select-all: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Resources; len(got) != 2 {
		t.Fatalf("select-all must keep locked: %v", got)
	}
}

func TestLockedBlocksForgetDisableRemove(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	if rec := doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"my-server"}`); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	lockPeer(t, d, "my-server", true)
	before := devConf(t, cfgPath)

	rec := doReq(t, d, "POST", "/tailnet/dev/forget", `{"peer":"my-server"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "is locked") {
		t.Fatalf("forget locked: %d %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, d, "POST", "/tailnet/dev/enabled", `{"on":false}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "locked selections") {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}

	// enabling is fine — the guard is one-directional
	if rec := doReq(t, d, "POST", "/tailnet/dev/enabled", `{"on":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, d, "DELETE", "/tailnet/dev", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "locked selections") {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}

	if got := devConf(t, cfgPath); len(got.Resources) != len(before.Resources) || len(got.Locked) != len(before.Locked) {
		t.Fatalf("a blocked mutation must not write: %v / %v", got.Resources, got.Locked)
	}

	// unlock reopens every door
	lockPeer(t, d, "my-server", false)
	if rec := doReq(t, d, "POST", "/tailnet/dev/enabled", `{"on":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable after unlock: %d %s", rec.Code, rec.Body.String())
	}
}

// A hand-edited config that lists a lock without the matching resource must
// still resolve the resource as selected (locked ⊆ selected, always).
func TestResolveSelectionsLockedAlwaysSelected(t *testing.T) {
	conf := TailnetConf{Name: "skynet", Locked: []string{"nucbox"}}
	st := mkStatus(peer("node-one", "nucbox."+skynetSuffix+".", "100.64.0.10"))

	resolved, errs, _ := resolveSelections(skynetSuffix, conf, st, nil, pinStore{})
	if len(errs) != 0 || len(resolved) != 1 || resolved[0].Short != "nucbox" {
		t.Fatalf("locked-but-not-listed must still select: resolved=%v errs=%v", resolved, errs)
	}
}

// /services reports locked entries as selected even when resources alone
// would say otherwise (same self-heal as resolveSelections).
func TestServicesSelectedIncludesLocked(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	stubServices(d, map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:web": {Name: "svc:web", DisplayName: "web"},
	})
	// a hand-edited config that lists a lock without the matching resource
	hand := `{"state_dir": "` + filepath.Dir(cfgPath) + `", "tailnets": [` +
		`{"name":"dev","cidr":"198.18.1.0/24","tun":"tsm0","locked":["svc:web"]}]}`
	if err := os.WriteFile(cfgPath, []byte(hand), 0600); err != nil {
		t.Fatal(err)
	}

	rec := doReq(t, d, "GET", "/services", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("services: %d %s", rec.Code, rec.Body.String())
	}
	var tss []tailnetServicesJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &tss); err != nil {
		t.Fatal(err)
	}
	if len(tss) != 1 || len(tss[0].Services) != 1 || !tss[0].Services[0].Selected {
		t.Fatalf("locked service must report selected: %s", rec.Body.String())
	}
}
