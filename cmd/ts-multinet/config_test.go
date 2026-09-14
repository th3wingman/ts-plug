package main

import "testing"

// The shipped example must load as-is: it is HuJSON (comments, trailing
// commas) and seeds /etc/ts-multinet/config.json on install. It ships with
// NOTHING configured — a fresh install starts empty (all-comments tailnets
// list) and tailnets are added live via the UI/CLI.
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := loadConfig("config.example.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tailnets) != 0 {
		t.Fatalf("example should ship empty, got tailnets: %+v", cfg.Tailnets)
	}
	// commented-out keys must stay at their zero values (defaults applied later)
	if cfg.MTU != 0 || cfg.DNSListen != "" || cfg.UIListen != "" || cfg.StateDir == "" {
		t.Fatalf("unexpected globals: %+v", cfg)
	}
}

// nodeHostname defaults to a slug of the tailnet name (ts-multinet-<slug>)
// so free-form names ("MS Infra") stay DNS-safe; an explicit hostname wins.
func TestNodeHostnameDefault(t *testing.T) {
	for _, tt := range []struct{ name, want string }{
		{"dev", "ts-multinet-dev"},
		{"MS Infra", "ts-multinet-ms-infra"},
		{"ms infra", "ts-multinet-ms-infra"},
		{"Ünïcode / names!!", "ts-multinet-n-code-names"},
	} {
		if got := (TailnetConf{Name: tt.name}).nodeHostname(); got != tt.want {
			t.Fatalf("nodeHostname(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
	tc := TailnetConf{Name: "dev", Hostname: "xps13"}
	if got := tc.nodeHostname(); got != "xps13" {
		t.Fatalf("explicit hostname = %q", got)
	}
	if got := (TailnetConf{Name: "My Corp Net"}).domainName(); got != "my-corp-net" {
		t.Fatalf("domainName fallback = %q", got)
	}
	if !validTailnetName("My Corp Net") || validTailnetName("../etc") || validTailnetName("") {
		t.Fatal("validTailnetName is wrong")
	}
}
