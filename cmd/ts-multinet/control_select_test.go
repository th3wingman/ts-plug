package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tailscale.com/tailcfg"
)

// select-all is the UI's "select all": one write that unions a whole set of
// resources and turns allow-all into an explicit list, so rows stay
// individually uncheckable afterwards.
func TestSelectAllUnionsAndClearsAllowAll(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	if rec := doReq(t, d, "POST", "/tailnet/dev/allow-all", `{"on":true}`); rec.Code != http.StatusOK {
		t.Fatalf("allow-all on: %d %s", rec.Code, rec.Body.String())
	}

	rec := doReq(t, d, "POST", "/tailnet/dev/select-all",
		`{"resources":["my-server","db","my-server"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("select-all: %d %s", rec.Code, rec.Body.String())
	}
	tc := devConf(t, cfgPath)
	if len(tc.Resources) != 2 || tc.Resources[0] != "my-server" || tc.Resources[1] != "db" {
		t.Fatalf("resources after select-all: %v", tc.Resources)
	}
	if tc.AllowAll {
		t.Fatal("select-all must clear allow_all so rows can be unchecked")
	}

	// repeating it is idempotent, and a later forget still removes one row
	if rec := doReq(t, d, "POST", "/tailnet/dev/select-all",
		`{"resources":["my-server","db"]}`); rec.Code != http.StatusOK {
		t.Fatalf("re-select-all: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Resources; len(got) != 2 {
		t.Fatalf("resources after re-select-all: %v", got)
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/forget", `{"peer":"db"}`); rec.Code != http.StatusOK {
		t.Fatalf("forget: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Resources; len(got) != 1 || got[0] != "my-server" {
		t.Fatalf("resources after forget: %v", got)
	}
}

func TestSelectAllRejectsBadRequests(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	before := len(devConf(t, cfgPath).Resources)

	for _, c := range []struct {
		name, body, want string
	}{
		{"empty", `{"resources":[]}`, "resources is required"},
		{"unknown peer", `{"resources":["nope"]}`, "unknown peer"},
		{"unknown service", `{"resources":["svc:ghost"]}`, "unknown service"},
	} {
		rec := doReq(t, d, "POST", "/tailnet/dev/select-all", c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want 400 %q", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	if got := devConf(t, cfgPath).Resources; len(got) != before {
		t.Fatalf("rejected select-all wrote to config: %v", got)
	}

	// needsLive, like /select: a tailnet in the config but not running is 404
	dir := filepath.Dir(cfgPath)
	parked := `{"state_dir": "` + dir + `", "tailnets": [` +
		`{"name":"dev","cidr":"198.18.1.0/24","tun":"tsm0"},` +
		`{"name":"prod","cidr":"198.18.2.0/24","tun":"tsm1"}]}`
	if err := os.WriteFile(cfgPath, []byte(parked), 0600); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, d, "POST", "/tailnet/prod/select-all", `{"resources":["my-server"]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("parked tailnet: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "in the config but not running") {
		t.Errorf("parked tailnet should say what to do: %s", rec.Body.String())
	}
}

// A bulk call validates services exactly like /select does, so one write can
// mix peers and advertised services.
func TestSelectAllAcceptsAdvertisedService(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	stubServices(d, map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:web": {Name: "svc:web", DisplayName: "web"},
	})

	rec := doReq(t, d, "POST", "/tailnet/dev/select-all",
		`{"resources":["my-server","svc:web"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("select-all: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Resources; len(got) != 2 || got[1] != "svc:web" {
		t.Fatalf("resources after select-all: %v", got)
	}
}
