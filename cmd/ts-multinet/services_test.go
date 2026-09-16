package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"tailscale.com/tailcfg"
)

func TestServiceNameHelpers(t *testing.T) {
	if got := serviceFQDN("svc:my-db", skynetSuffix); got != "my-db."+skynetSuffix {
		t.Errorf("serviceFQDN = %q", got)
	}
	if got := serviceFQDN("svc:my-db", ""); got != "my-db" {
		t.Errorf("serviceFQDN without suffix = %q", got)
	}
	// the friendly alias keeps the label, drops the svc: prefix; peers pass through
	if got := resourceAlias("svc:my-db", "skynet"); got != "my-db.skynet" {
		t.Errorf("resourceAlias(service) = %q", got)
	}
	if got := resourceAlias("nucbox", "skynet"); got != "nucbox.skynet" {
		t.Errorf("resourceAlias(peer) = %q", got)
	}
}

func TestResolveSelectionsServices(t *testing.T) {
	conf := TailnetConf{Name: "skynet", Resources: []string{"nucbox", "svc:my-db", "svc:ghost"}}
	st := mkStatus(peer("node-one", "nucbox."+skynetSuffix+".", "100.64.0.10"))
	svcs := map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:my-db": {Name: "svc:my-db", DisplayName: "my database", Addrs: []netip.Addr{netip.MustParseAddr("100.64.0.50")}},
	}

	pins := pinStore{}
	resolved, errs, changed := resolveSelections(skynetSuffix, conf, st, svcs, pins)
	if !changed || len(resolved) != 2 || len(errs) != 1 {
		t.Fatalf("resolved=%v errs=%v changed=%v", resolved, errs, changed)
	}
	if !strings.Contains(errs[0], "svc:ghost") || !strings.Contains(errs[0], "advertised service") {
		t.Errorf("missing service must be loud and name the kind: %q", errs[0])
	}
	svc := resolved[1]
	if svc.Short != "svc:my-db" || svc.FQDN != "my-db."+skynetSuffix || svc.Addr != "100.64.0.50" {
		t.Fatalf("service record = %+v", svc)
	}
	// services are not identity-pinned: the svc: name is the identity
	if _, pinned := pins["svc:my-db"]; pinned {
		t.Fatalf("service must not be pinned: %+v", pins)
	}

	// second run is a stable no-op (peer pin already recorded)
	_, _, changed = resolveSelections(skynetSuffix, conf, st, svcs, pins)
	if changed {
		t.Fatal("service re-resolve must not churn pins")
	}
}

// A peer and a service resolve to distinct synthetic IPs, and both alias and
// MagicDNS spellings of a service share the one canonical service IP.
func TestServiceAndPeerGetDistinctSyntheticIPs(t *testing.T) {
	reg, err := newRegistry([]TailnetConf{
		{Name: "skynet", Domain: "lan", CIDR: "198.18.1.0/24", TUN: "tsm0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg.registerSuffix("skynet", skynetSuffix)

	peerFQDN := "nucbox." + skynetSuffix
	svcFQDN := "my-db." + skynetSuffix
	reg.seed("skynet", []string{peerFQDN, svcFQDN})

	peerIP, ok := reg.allocate("skynet", peerFQDN)
	if !ok {
		t.Fatal("peer allocation failed")
	}
	svcIP, ok := reg.allocate("skynet", svcFQDN)
	if !ok {
		t.Fatal("service allocation failed")
	}
	if peerIP.Equal(svcIP) {
		t.Fatalf("peer and service share a synthetic IP: %s", peerIP)
	}

	// the friendly alias canonicalizes to the same service FQDN...
	tn, fqdn, auth, ok := reg.match("my-db.lan.")
	if !ok || tn != "skynet" || fqdn != svcFQDN || auth {
		t.Fatalf("service alias match: %q %q auth=%v %v", tn, fqdn, auth, ok)
	}
	// ...and therefore maps to the same single synthetic IP.
	if ip, _ := reg.allocate(tn, fqdn); !ip.Equal(svcIP) {
		t.Fatalf("alias got a different IP: %s vs %s", ip, svcIP)
	}
}

// stubServices installs a fake advertised-service list on a stub daemon.
func stubServices(d *Daemon, svcs map[tailcfg.ServiceName]tailcfg.ServiceDetails) {
	d.services = func(context.Context, *Tailnet) (map[tailcfg.ServiceName]tailcfg.ServiceDetails, error) {
		return svcs, nil
	}
}

func TestHandleServicesAndSelectService(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	stubServices(d, map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:my-db": {
			Name:        "svc:my-db",
			DisplayName: "my database",
			Addrs:       []netip.Addr{netip.MustParseAddr("100.64.0.50")},
			Ports:       []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 5432, Last: 5432}}},
		},
	})

	list := func() tailnetServicesJSON {
		t.Helper()
		rec := doReq(t, d, "GET", "/services", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /services = %d %s", rec.Code, rec.Body.String())
		}
		var tss []tailnetServicesJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &tss); err != nil {
			t.Fatal(err)
		}
		if len(tss) != 1 || len(tss[0].Services) != 1 {
			t.Fatalf("services reply = %+v", tss)
		}
		return tss[0]
	}

	svc := list().Services[0]
	if svc.Name != "svc:my-db" || svc.DisplayName != "my database" || svc.VIPs[0] != "100.64.0.50" || svc.Ports[0] != "tcp:5432" || svc.Selected {
		t.Fatalf("service json = %+v", svc)
	}

	rec := doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"svc:my-db"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("select svc = %d %s", rec.Code, rec.Body.String())
	}
	if tc := devConf(t, cfgPath); !slices.Contains(tc.Resources, "svc:my-db") {
		t.Fatalf("service not persisted: %+v", tc.Resources)
	}
	if !list().Services[0].Selected {
		t.Fatal("selected service must report selected")
	}

	// an unknown svc: name is a 400 whose text names the kind
	rec = doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"svc:nope"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown service") {
		t.Fatalf("unknown service = %d %s", rec.Code, rec.Body.String())
	}
}
