package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
)

// auto-select rules are managed like any other selection knob: one
// comment-preserving write per change, well-formedness enforced at the
// handler (a typo'd rule is a 400, never a silent no-op), and a reply that
// names what the rules currently match.
func TestAutoAddRemoveClear(t *testing.T) {
	d, cfgPath := stubDaemon(t)

	rec := doReq(t, d, "POST", "/tailnet/dev/auto", `{"resources":["prod"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "must start with") {
		t.Fatalf("bad rule = %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).AutoSelect; len(got) != 0 {
		t.Fatalf("rejected rule wrote to config: %v", got)
	}

	rec = doReq(t, d, "POST", "/tailnet/dev/auto", `{"resources":["tag:infra","svc:prod-*"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add rules: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).AutoSelect; !slices.Equal(got, []string{"tag:infra", "svc:prod-*"}) {
		t.Fatalf("auto_select after add: %v", got)
	}

	// re-adding the same rule is idempotent, and case folds to the canonical form
	rec = doReq(t, d, "POST", "/tailnet/dev/auto", `{"resources":["TAG:infra"]}`)
	if rec.Code != http.StatusOK || len(devConf(t, cfgPath).AutoSelect) != 2 {
		t.Fatalf("re-add not idempotent: %d %s", rec.Code, rec.Body.String())
	}

	// off with rules removes those; bare off clears all and drops the member
	rec = doReq(t, d, "POST", "/tailnet/dev/auto", `{"on":false,"resources":["tag:infra"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove rule: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).AutoSelect; !slices.Equal(got, []string{"svc:prod-*"}) {
		t.Fatalf("auto_select after remove: %v", got)
	}
	rec = doReq(t, d, "POST", "/tailnet/dev/auto", `{"on":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear rules: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).AutoSelect; len(got) != 0 {
		t.Fatalf("auto_select after clear: %v", got)
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"auto_select"`) {
		t.Errorf("cleared auto_select member survived in the file:\n%s", b)
	}
}

// the mutation reply names the rules and what they currently select — the
// one place a typo'd-but-well-formed glob (matches nothing) still shows up.
func TestAutoNoteNamesCurrentMatches(t *testing.T) {
	d, _ := stubDaemon(t)
	stubServices(d, map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:prod-db": {Name: "svc:prod-db", DisplayName: "prod db"},
	})
	d.peerTagsByShort = func(context.Context, *Tailnet) (map[string][]string, error) {
		return map[string][]string{"my-server": {"tag:infra"}}, nil
	}

	rec := doReq(t, d, "POST", "/tailnet/dev/auto", `{"resources":["tag:infra","svc:prod-*"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"tag:infra, svc:prod-*", "my-server", "svc:prod-db"} {
		if !strings.Contains(body, want) {
			t.Errorf("reply missing %q: %s", want, body)
		}
	}
}

// forget refuses a name a rule would re-add on the next apply, and names
// the rule — a forget that silently un-forgets itself would look broken.
func TestForgetRefusesAutoSelected(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	stubServices(d, map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:prod-db": {Name: "svc:prod-db", Ports: []tailcfg.ProtoPortRange{{Proto: 6, Ports: tailcfg.PortRange{First: 3306, Last: 3306}}}},
	})
	d.peerTagsByShort = func(context.Context, *Tailnet) (map[string][]string, error) {
		return map[string][]string{"my-server": {"tag:infra"}}, nil
	}
	doReq(t, d, "POST", "/tailnet/dev/auto", `{"resources":["tag:infra","svc:prod-*"]}`)
	// both are also explicit selections, so forget has something to remove
	doReq(t, d, "POST", "/tailnet/dev/select-all", `{"resources":["my-server","svc:prod-db"]}`)

	rec := doReq(t, d, "POST", "/tailnet/dev/forget", `{"peer":"my-server"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "auto-selected by tag:infra") {
		t.Fatalf("peer forget = %d %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, d, "POST", "/tailnet/dev/forget", `{"peer":"svc:prod-db"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "auto-selected by svc:prod-*") {
		t.Fatalf("service forget = %d %s", rec.Code, rec.Body.String())
	}

	// an untagged peer forgets normally, the rule-matched ones forget once
	// the rule is gone, and a service matched only by its advertised port
	// refuses the same way
	if rec := doReq(t, d, "POST", "/tailnet/dev/forget", `{"peer":"db"}`); rec.Code != http.StatusOK {
		t.Fatalf("untagged forget = %d %s", rec.Code, rec.Body.String())
	}
	doReq(t, d, "POST", "/tailnet/dev/auto", `{"on":false,"resources":["tag:infra","svc:prod-*"]}`)
	doReq(t, d, "POST", "/tailnet/dev/auto", `{"resources":["tcp:3306"]}`)
	rec = doReq(t, d, "POST", "/tailnet/dev/forget", `{"peer":"svc:prod-db"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "auto-selected by tcp:3306") {
		t.Fatalf("port-rule forget = %d %s", rec.Code, rec.Body.String())
	}
	doReq(t, d, "POST", "/tailnet/dev/auto", `{"on":false}`)
	if rec := doReq(t, d, "POST", "/tailnet/dev/forget", `{"peer":"my-server"}`); rec.Code != http.StatusOK {
		t.Fatalf("forget after rule removal = %d %s", rec.Code, rec.Body.String())
	}
	if slices.Contains(devConf(t, cfgPath).Resources, "my-server") {
		t.Fatal("my-server still in resources after forget")
	}
}

// clear-all empties resources and turns off allow_all; auto rules go too —
// a rule left behind would re-select everything on the next apply.
func TestClearClearsAutoRules(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	doReq(t, d, "POST", "/tailnet/dev/auto", `{"resources":["tag:infra"]}`)

	rec := doReq(t, d, "POST", "/tailnet/dev/clear", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	tc := devConf(t, cfgPath)
	if len(tc.AutoSelect) != 0 || len(tc.Resources) != 0 || tc.AllowAll {
		t.Fatalf("clear left selection state behind: %+v", tc)
	}
}

// the services listing derives Selected from the effective selection, so a
// glob-matched service reports selected without ever being listed.
func TestServicesSelectedByRule(t *testing.T) {
	d, _ := stubDaemon(t)
	stubServices(d, map[tailcfg.ServiceName]tailcfg.ServiceDetails{
		"svc:prod-db": {Name: "svc:prod-db"},
		"svc:web":     {Name: "svc:web"},
	})
	doReq(t, d, "POST", "/tailnet/dev/auto", `{"resources":["svc:prod-*"]}`)

	rec := doReq(t, d, "GET", "/services", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("services: %d %s", rec.Code, rec.Body.String())
	}
	var tss []tailnetServicesJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &tss); err != nil {
		t.Fatal(err)
	}
	byName := map[string]bool{}
	for _, s := range tss[0].Services {
		byName[s.Name] = s.Selected
	}
	if !byName["svc:prod-db"] || byName["svc:web"] {
		t.Fatalf("Selected must follow the rule: %v", byName)
	}
}

// only notifications that can change what a rule matches re-apply: full
// peer add/replace/remove (tag changes ride whole nodes) and self changes
// (advertised-service visibility); online/offline flaps are patches, never
// carry tags, and must not trigger.
func TestAutoNotifyApplies(t *testing.T) {
	node := &tailcfg.Node{}
	cases := []struct {
		n    ipn.Notify
		want bool
	}{
		{ipn.Notify{}, false},
		{ipn.Notify{SelfChange: node}, true},
		{ipn.Notify{PeersChanged: []*tailcfg.Node{node}}, true},
		{ipn.Notify{PeersRemoved: []tailcfg.NodeID{1}}, true},
		{ipn.Notify{PeerChangedPatch: []*tailcfg.PeerChange{{NodeID: 1}}}, false},
		// a patch riding along a real change still triggers: the peer change is the signal, the patch is noise
		{ipn.Notify{PeerChangedPatch: []*tailcfg.PeerChange{{NodeID: 1}}, PeersRemoved: []tailcfg.NodeID{1}}, true},
	}
	for _, c := range cases {
		if got := autoNotifyApplies(c.n); got != c.want {
			t.Errorf("autoNotifyApplies(%+v) = %v, want %v", c.n, got, c.want)
		}
	}
}
