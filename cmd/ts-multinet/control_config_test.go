package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stubDaemon builds a Daemon around a commented config whose tailnets have no
// live tsnet: status is in-memory and never Running, so applySelections just
// skips them and writes an empty hosts block. peerShorts is stubbed with the
// names `peers` would show.
func stubDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.jsonc")
	src := `{
	  "state_dir": "` + dir + `",
	  // comments must survive daemon edits
	  "tailnets": [
	    { "name": "dev", "cidr": "198.18.1.0/24", "tun": "tsm0" },
	  ]
	}`
	if err := os.WriteFile(cfgPath, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hosts"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	reg, err := newRegistry([]TailnetConf{{Name: "dev", CIDR: "198.18.1.0/24", TUN: "tsm0"}})
	if err != nil {
		t.Fatal(err)
	}
	conf := TailnetConf{Name: "dev", CIDR: "198.18.1.0/24", TUN: "tsm0"}
	d := newDaemon([]*Tailnet{{conf: conf}}, reg, cfgPath, filepath.Join(dir, "hosts"))
	d.peerShorts = func(context.Context, *Tailnet) ([]string, error) {
		return []string{"my-server", "db"}, nil
	}
	return d, cfgPath
}

func doReq(t *testing.T, d *Daemon, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	d.controlMux().ServeHTTP(rec, req)
	return rec
}

func devConf(t *testing.T, cfgPath string) *TailnetConf {
	t.Helper()
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	tc := confByName(cfg, "dev")
	if tc == nil {
		t.Fatal("dev missing from config")
	}
	return tc
}

func TestSelectUpdatesConfigPreservingComments(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	rec := doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"my-server"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Resources; len(got) != 1 || got[0] != "my-server" {
		t.Fatalf("resources after select: %v", got)
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "// comments must survive daemon edits") {
		t.Errorf("comment lost:\n%s", b)
	}

	// selecting the same peer twice is idempotent
	rec = doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"my-server"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-select: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Resources; len(got) != 1 {
		t.Fatalf("resources after re-select: %v", got)
	}
}

func TestSelectRejectsUnknownPeerAndTailnet(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	before := devConf(t, cfgPath).Resources

	rec := doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown peer: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Resources; len(got) != len(before) {
		t.Fatalf("config changed on rejected select")
	}

	rec = doReq(t, d, "POST", "/tailnet/prod/select", `{"peer":"my-server"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown tailnet: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "known: dev") {
		t.Errorf("404 should list known tailnets: %s", rec.Body.String())
	}

	// a parked tailnet (in the config, not running) gets the actionable message
	dir := filepath.Dir(cfgPath)
	parked := `{"state_dir": "` + dir + `", "tailnets": [` +
		`{"name":"dev","cidr":"198.18.1.0/24","tun":"tsm0"},` +
		`{"name":"prod","cidr":"198.18.2.0/24","tun":"tsm1"}]}`
	if err := os.WriteFile(cfgPath, []byte(parked), 0600); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, d, "POST", "/tailnet/prod/select", `{"peer":"my-server"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("parked tailnet: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "in the config but not running") {
		t.Errorf("parked tailnet should say what to do: %s", rec.Body.String())
	}

	rec = doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty peer: %d %s", rec.Code, rec.Body.String())
	}
}

func TestForgetRemovesResource(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	if rec := doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"my-server"}`); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"db"}`); rec.Code != http.StatusOK {
		t.Fatalf("select: %d %s", rec.Code, rec.Body.String())
	}

	rec := doReq(t, d, "POST", "/tailnet/dev/forget", `{"peer":"my-server"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("forget: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Resources; len(got) != 1 || got[0] != "db" {
		t.Fatalf("resources after forget: %v", got)
	}

	// forgetting a name that isn't selected is a no-op success (stale cleanup)
	if rec := doReq(t, d, "POST", "/tailnet/dev/forget", `{"peer":"ghost"}`); rec.Code != http.StatusOK {
		t.Fatalf("forget ghost: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAllowAllToggle(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	if rec := doReq(t, d, "POST", "/tailnet/dev/allow-all", `{"on":true}`); rec.Code != http.StatusOK {
		t.Fatalf("allow-all on: %d %s", rec.Code, rec.Body.String())
	}
	if !devConf(t, cfgPath).AllowAll {
		t.Fatal("allow_all not set")
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/allow-all", `{"on":false}`); rec.Code != http.StatusOK {
		t.Fatalf("allow-all off: %d %s", rec.Code, rec.Body.String())
	}
	if devConf(t, cfgPath).AllowAll {
		t.Fatal("allow_all not cleared")
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/allow-all", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing on: %d %s", rec.Code, rec.Body.String())
	}
}

// native_dns is hosts-block-only: it changes what shells complete against,
// never DNS resolution (the responder answers both spellings regardless).
func TestNativeDNSToggle(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	if rec := doReq(t, d, "POST", "/tailnet/dev/native-dns", `{"on":true}`); rec.Code != http.StatusOK {
		t.Fatalf("native-dns on: %d %s", rec.Code, rec.Body.String())
	}
	if !devConf(t, cfgPath).NativeDNS {
		t.Fatal("native_dns not set")
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/native-dns", `{"on":false}`); rec.Code != http.StatusOK {
		t.Fatalf("native-dns off: %d %s", rec.Code, rec.Body.String())
	}
	if devConf(t, cfgPath).NativeDNS {
		t.Fatal("native_dns not cleared")
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/native-dns", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing on: %d %s", rec.Code, rec.Body.String())
	}
}

// A stored auth key never leaves the daemon: GET /config serves it redacted,
// and later mutations (which load the real file) cannot wipe it.
func TestAuthKeyRedactedAndSurvives(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	d.rt.ctx = context.Background()
	d.rt.cleanupTUN = func(cidr, dev string) {}
	d.rt.starter = func(ctx context.Context, tc TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
		return &Tailnet{conf: tc, wg: &sync.WaitGroup{}}, nil
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/authkey", `{"auth_key":"tskey-auth-secret123"}`); rec.Code != http.StatusOK {
		t.Fatalf("authkey = %d %s", rec.Code, rec.Body.String())
	}
	if devConf(t, cfgPath).AuthKey != "tskey-auth-secret123" {
		t.Fatal("auth_key not stored")
	}
	rec := doReq(t, d, "GET", "/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("config = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "tskey-auth-secret123") {
		t.Fatal("GET /config leaked the auth key")
	}
	if !strings.Contains(rec.Body.String(), authKeyRedacted) {
		t.Fatal("GET /config missing the redaction marker")
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/domain", `{"domain":"lab.corp"}`); rec.Code != http.StatusOK {
		t.Fatalf("domain = %d %s", rec.Code, rec.Body.String())
	}
	if devConf(t, cfgPath).AuthKey != "tskey-auth-secret123" {
		t.Fatal("a later mutation wiped the stored auth key")
	}
}

// Adding a tailnet with an auth key stores it (one-step tagged enrollment)
// and the response must not echo the key back.
func TestAddTailnetWithAuthKey(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	rec := doReq(t, d, "POST", "/tailnet", `{"name":"acme","auth_key":"tskey-auth-k3"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tskey-auth-k3") {
		t.Fatal("add response echoed the auth key")
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if tc := confByName(cfg, "acme"); tc == nil || tc.AuthKey != "tskey-auth-k3" {
		t.Fatalf("auth_key not stored on add: %+v", tc)
	}
}

func TestDomainSetValidateAndClear(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	if rec := doReq(t, d, "POST", "/tailnet/dev/domain", `{"domain":"lab.corp"}`); rec.Code != http.StatusOK {
		t.Fatalf("domain set: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Domain; got != "lab.corp" {
		t.Fatalf("domain: %q", got)
	}
	if got := devConf(t, cfgPath).domainName(); got != "lab.corp" {
		t.Fatalf("domainName: %q", got)
	}

	if rec := doReq(t, d, "POST", "/tailnet/dev/domain", `{"domain":"Bad"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad domain: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Domain; got != "lab.corp" {
		t.Fatalf("rejected domain still applied: %q", got)
	}

	// empty clears the override; domainName falls back to the tailnet name
	if rec := doReq(t, d, "POST", "/tailnet/dev/domain", `{"domain":""}`); rec.Code != http.StatusOK {
		t.Fatalf("domain clear: %d %s", rec.Code, rec.Body.String())
	}
	if got := devConf(t, cfgPath).Domain; got != "" {
		t.Fatalf("domain not cleared: %q", got)
	}
	if got := devConf(t, cfgPath).domainName(); got != "dev" {
		t.Fatalf("domainName fallback: %q", got)
	}
}

func TestGetConfig(t *testing.T) {
	d, cfgPath := stubDaemon(t)
	rec := doReq(t, d, "GET", "/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get config: %d %s", rec.Code, rec.Body.String())
	}
	var cfg Config
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tailnets) != 1 || cfg.Tailnets[0].Name != "dev" {
		t.Fatalf("config tailnets: %+v", cfg.Tailnets)
	}
	if cfg.StateDir != filepath.Dir(cfgPath) {
		t.Fatalf("state_dir: %q", cfg.StateDir)
	}
}

func TestLoginErrorListsTailnets(t *testing.T) {
	d, _ := stubDaemon(t)
	rec := doReq(t, d, "POST", "/login", `{"tailnet":"nope"}`)
	if rec.Code != http.StatusOK { // login replies 200 with an Error field
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var res loginResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Error, "no such tailnet (known: dev)") {
		t.Errorf("login error: %q", res.Error)
	}
}

// /domain-hosts is a config-only mutation like /native-dns: it patches the
// file in place (comments survive) and needs no live tailnet.
func TestDomainHostsToggle(t *testing.T) {
	d, cfgPath := stubDaemon(t)

	rec := doReq(t, d, "POST", "/tailnet/dev/domain-hosts", `{"on":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("domain-hosts on: %d %s", rec.Code, rec.Body.String())
	}
	if !devConf(t, cfgPath).DomainHosts {
		t.Fatal("domain_hosts not persisted")
	}
	rec = doReq(t, d, "POST", "/tailnet/dev/domain-hosts", `{"on":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("domain-hosts off: %d %s", rec.Code, rec.Body.String())
	}
	if devConf(t, cfgPath).DomainHosts {
		t.Fatal("domain_hosts not cleared")
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "// comments must survive daemon edits") {
		t.Errorf("comment lost:\n%s", b)
	}
}
