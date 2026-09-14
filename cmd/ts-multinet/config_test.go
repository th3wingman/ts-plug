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
