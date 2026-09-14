package main

import "testing"

// The shipped example must load as-is: it is HuJSON (comments, trailing
// commas) and seeds /etc/ts-multinet/config.json on install.
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := loadConfig("config.example.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tailnets) != 1 || cfg.Tailnets[0].Name != "example" {
		t.Fatalf("unexpected tailnets: %+v", cfg.Tailnets)
	}
	// commented-out keys must stay at their zero values (defaults applied later)
	if cfg.MTU != 0 || cfg.DNSListen != "" || cfg.Tailnets[0].AllowAll {
		t.Fatalf("commented-out keys leaked into config: %+v", cfg)
	}
}
