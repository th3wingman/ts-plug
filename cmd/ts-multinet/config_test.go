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

// nodeHostname defaults to the historical ts-multinet-<name> scheme so
// existing configs keep their admin-console device names.
func TestNodeHostnameDefault(t *testing.T) {
	tc := TailnetConf{Name: "dev"}
	if got := tc.nodeHostname(); got != "ts-multinet-dev" {
		t.Fatalf("default hostname = %q", got)
	}
	tc.Hostname = "xps13"
	if got := tc.nodeHostname(); got != "xps13" {
		t.Fatalf("explicit hostname = %q", got)
	}
}
