package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusDNSRegistered covers the per-tailnet dns_registered flag: true only
// when resolvedSync holds the tailnet's tun, false without one (containers,
// stubs, and hosts where resolved never came up).
func TestStatusDNSRegistered(t *testing.T) {
	d := &Daemon{tailnets: []*Tailnet{
		{
			conf:     TailnetConf{Name: "dev", CIDR: "198.18.1.0/24", TUN: "tsm0"},
			dev:      "tsm0",
			resolved: &resolvedSync{registered: map[string]string{"tsm0": "tail523555.ts.net dev"}},
		},
		{
			conf: TailnetConf{Name: "corp", CIDR: "198.18.2.0/24", TUN: "tsm1"},
			dev:  "tsm1", // no resolvedSync: hosts without resolved, or containers
		},
	}}

	rec := doReq(t, d, "GET", "/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var sts []tailnetStatusJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &sts); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range sts {
		got[s.Name] = s.DNSRegistered
	}
	if len(got) != 2 || !got["dev"] || got["corp"] {
		t.Fatalf("dns_registered = %+v, want dev true / corp false", got)
	}
}

func updateConfig(t *testing.T, d *Daemon, body string) (int, string) {
	t.Helper()
	rec := doReq(t, d, "POST", "/config", body)
	return rec.Code, rec.Body.String()
}

func TestUpdateConfigGlobals(t *testing.T) {
	d, cfgPath := stubDaemon(t)

	code, body := updateConfig(t, d, `{"mtu": 1500, "dns_listen": "127.0.0.1:5353"}`)
	if code != http.StatusOK {
		t.Fatalf("update: %d %s", code, body)
	}
	var res struct {
		OK           string `json:"ok"`
		NeedsRestart string `json:"needs_restart"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	if res.NeedsRestart != "mtu, dns_listen" {
		t.Fatalf("needs_restart = %q, want %q", res.NeedsRestart, "mtu, dns_listen")
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MTU != 1500 || cfg.DNSListen != "127.0.0.1:5353" {
		t.Fatalf("globals not applied: mtu=%d dns_listen=%q", cfg.MTU, cfg.DNSListen)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "// comments must survive daemon edits") {
		t.Fatalf("comment lost:\n%s", raw)
	}
	// new globals land above the tailnets array, keeping the file readable
	if i, j := strings.Index(string(raw), `"mtu"`), strings.Index(string(raw), `"tailnets"`); i < 0 || j < 0 || i > j {
		t.Fatalf("globals should be written above tailnets:\n%s", raw)
	}

	// the on-disk file stays the source of truth for GET /config
	rec := doReq(t, d, "GET", "/config", "")
	var got Config
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.MTU != 1500 || got.DNSListen != "127.0.0.1:5353" {
		t.Fatalf("GET /config disagrees: %+v", got)
	}
}

func TestUpdateConfigNoOpLeavesFileAlone(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	code, body := updateConfig(t, d, `{}`)
	if code != http.StatusOK {
		t.Fatalf("no-op: %d %s", code, body)
	}
	if !strings.Contains(body, `"needs_restart":""`) {
		t.Fatalf("no-op needs_restart should be empty: %s", body)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("no-op rewrote the file:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// setting a global to the value it already has is also a no-op
	if code, body := updateConfig(t, d, `{"mtu": 1500}`); code != http.StatusOK {
		t.Fatalf("set mtu: %d %s", code, body)
	}
	if code, body := updateConfig(t, d, `{"mtu": 1500}`); code != http.StatusOK || !strings.Contains(body, `"needs_restart":""`) {
		t.Fatalf("same-value set should report no change: %d %s", code, body)
	}
}

func TestUpdateConfigClearsOverrides(t *testing.T) {
	d, cfgPath := stubDaemon(t)

	if code, body := updateConfig(t, d, `{"upstream_dns": "1.1.1.1:53", "mtu": 1500}`); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, body)
	}
	// clearing falls back to the daemon defaults and drops the member
	code, body := updateConfig(t, d, `{"upstream_dns": "", "mtu": 0}`)
	if code != http.StatusOK {
		t.Fatalf("clear: %d %s", code, body)
	}
	// change order follows the request order: mtu, dns_listen, upstream_dns, …
	if !strings.Contains(body, `"needs_restart":"mtu, upstream_dns"`) {
		t.Fatalf("clear needs_restart: %s", body)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamDNS != "" || cfg.MTU != 0 {
		t.Fatalf("overrides not cleared: upstream=%q mtu=%d", cfg.UpstreamDNS, cfg.MTU)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{`"upstream_dns"`, `"mtu"`} {
		if strings.Contains(string(raw), member) {
			t.Fatalf("cleared %s should be removed from the file:\n%s", member, raw)
		}
	}
}

func TestUpdateConfigRejectsStateDir(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"state_dir": "/tmp/elsewhere"}`, `{"state_dir": ""}`} {
		code, got := updateConfig(t, d, body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s, want 400", body, code, got)
		}
		if !strings.Contains(got, "manual-only") {
			t.Fatalf("%s: error should point at manual editing: %s", body, got)
		}
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected request must not touch the file")
	}
}

func TestUpdateConfigRejectsInvalidValues(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	for _, body := range []string{
		`{"mtu": 100}`,                        // below the WireGuard minimum
		`{"mtu": 9001}`,                       // above the jumbo ceiling
		`{"dns_listen": "127.0.0.1"}`,         // no port
		`{"ui_listen": "nope"}`,               // not host:port
		`{"ui_listen": ":8123"}`,              // empty host is ambiguous
		`{"upstream_dns": "999.999.999.999"}`, // neither IP nor host:port
		`{"hosts_file": "relative/hosts"}`,    // would resolve against the daemon's cwd
	} {
		code, got := updateConfig(t, d, body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s, want 400", body, code, got)
		}
	}
	// nothing rejected above may have been written
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MTU != 0 || cfg.DNSListen != "" || cfg.UIListen != "" || cfg.UpstreamDNS != "" || cfg.HostsFile != "" {
		t.Fatalf("rejected values leaked into the config: %+v", cfg)
	}
}

func TestValidGlobalsHelpers(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:53", "0.0.0.0:8123", "[::1]:8123", "dns.example:53"} {
		if !validListenAddr(ok) {
			t.Fatalf("validListenAddr(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "127.0.0.1", ":53", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:abc"} {
		if validListenAddr(bad) {
			t.Fatalf("validListenAddr(%q) = true", bad)
		}
	}
	for _, ok := range []string{"1.1.1.1", "fd71:34ba:f0d6::1", "1.1.1.1:5353", "[fd71::1]:53"} {
		if !validUpstreamDNS(ok) {
			t.Fatalf("validUpstreamDNS(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "nope", "999.999.999.999"} {
		if validUpstreamDNS(bad) {
			t.Fatalf("validUpstreamDNS(%q) = true", bad)
		}
	}
	if !filepath.IsAbs("/etc/hosts") {
		t.Fatal("test premise: /etc/hosts is absolute")
	}
}
