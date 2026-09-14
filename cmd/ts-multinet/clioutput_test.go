package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The --details human formatters must surface the fields the plain tables
// omit (hostname/login URL for status, FQDN for peers, raw fields for check),
// and marshalPretty must round-trip what --json emits.
func TestFormattersAndJSONOutput(t *testing.T) {
	sts := []tailnetStatusJSON{{
		Name: "skynet", Suffix: "tail95e9d1.ts.net", CIDR: "198.18.1.0/24",
		Hostname: "xps13-m", AssignedIP: "100.76.67.42", State: "Running",
		LoginURL: "https://login.tailscale.com/a/x", Selected: 11, Peers: 29, Up: 22,
	}}
	cfg := Config{Tailnets: []TailnetConf{{Name: "skynet", Domain: "skynet"}}}

	d := formatStatusDetails(sts, cfg)
	for _, want := range []string{"skynet — Running", "hostname  xps13-m", "domain    skynet", "login     https://login.tailscale.com/a/x", "11 selected"} {
		if !strings.Contains(d, want) {
			t.Errorf("status details missing %q:\n%s", want, d)
		}
	}
	if strings.Contains(formatStatusTable(sts), "xps13-m") {
		t.Error("plain status table must stay compact (no hostname)")
	}

	tps := []tailnetPeersJSON{{
		Name: "skynet", Suffix: "tail95e9d1.ts.net", Up: 1,
		Peers: []peerJSON{{
			Name: "nucbox", FQDN: "nucbox.tail95e9d1.ts.net", IP: "100.114.222.31",
			OS: "linux", Online: true, Services: []int{22, 443},
		}},
	}}
	pd := formatPeersDetails(tps, "")
	for _, want := range []string{"nucbox.tail95e9d1.ts.net", "linux", ":22 :443"} {
		if !strings.Contains(pd, want) {
			t.Errorf("peers details missing %q:\n%s", want, pd)
		}
	}

	chk := checkJSON{Host: "nucbox.skynet", Tailnet: "skynet", Suffix: "tail95e9d1.ts.net",
		ResolvedIP: "198.18.1.5", Result: "open", LatencyMS: 340, Banner: "SSH-2.0-OpenSSH"}
	cd := formatCheckDetails(chk)
	for _, want := range []string{"198.18.1.5", "340ms", "SSH-2.0-OpenSSH", "tail95e9d1.ts.net"} {
		if !strings.Contains(cd, want) {
			t.Errorf("check details missing %q:\n%s", want, cd)
		}
	}

	// the --json path: marshalPretty output parses back to the same data
	var back []tailnetStatusJSON
	if err := json.Unmarshal([]byte(marshalPretty(sts)), &back); err != nil {
		t.Fatalf("marshalPretty not valid JSON: %v", err)
	}
	if back[0].Hostname != "xps13-m" || back[0].LoginURL != "https://login.tailscale.com/a/x" {
		t.Fatalf("json round-trip lost fields: %+v", back[0])
	}
}
