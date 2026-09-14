package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncStub is the observable state of the stubbed starter: which confs
// started, and a channel that receives each stop. The stop signal is the
// starter's cancel — in production that is what winds down the watcher and
// the forwarder, so seeing it fire is the teardown test.
type syncStub struct {
	mu      sync.Mutex
	started map[string]TailnetConf
	stopped chan string
}

func (s *syncStub) record(tc TailnetConf, ctx context.Context) {
	// the starter's cancel cancels ctx; Done firing is the teardown signal —
	// in production that is what winds down the watcher and the forwarder.
	go func() {
		<-ctx.Done()
		s.stopped <- tc.Name
	}()
	s.mu.Lock()
	s.started[tc.Name] = tc
	s.mu.Unlock()
}

// confs returns the confs the stub starter has seen, by name.
func (s *syncStub) confs() map[string]TailnetConf {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]TailnetConf, len(s.started))
	for k, v := range s.started {
		out[k] = v
	}
	return out
}

func waitStopped(t *testing.T, s *syncStub, want string) {
	t.Helper()
	select {
	case name := <-s.stopped:
		if name != want {
			t.Fatalf("stopped %q, want %q", name, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("tailnet %q was never stopped", want)
	}
}

// syncDaemon is stubDaemon's runtime-lifecycle counterpart: the registry
// starts empty and tailnets come up through syncTailnets with a stubbed
// starter (no tsnet, no TUN), so starts/stops are observable without root.
// Tailnet state is "" (never Running), so applySelections skips them and the
// hosts block stays empty.
func syncDaemon(t *testing.T) (*Daemon, string, *syncStub) {
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
	reg, err := newRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := newDaemon(nil, reg, cfgPath, filepath.Join(dir, "hosts"))
	d.rt.ctx = context.Background()
	d.rt.baseDir = dir
	stub := &syncStub{started: map[string]TailnetConf{}, stopped: make(chan string, 16)}
	d.rt.starter = func(ctx context.Context, tc TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
		cctx, cancel := context.WithCancel(ctx)
		stub.record(tc, cctx)
		return &Tailnet{conf: tc, cancel: cancel}, nil
	}
	t.Cleanup(func() { // no leaked stub tailnets
		for _, tn := range d.liveTailnets() {
			tn.Close()
		}
	})
	if err := d.syncTailnets(mustLoad(t, cfgPath)); err != nil {
		t.Fatal(err)
	}
	return d, cfgPath, stub
}

func TestAddTailnetEndpointPicksDefaults(t *testing.T) {
	d, cfgPath, stub := syncDaemon(t)

	rec := doReq(t, d, "POST", "/tailnet", `{"name": "corp"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add tailnet: %d %s", rec.Code, rec.Body.String())
	}
	var res map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res["cidr"] != "198.18.2.0/24" || res["tun"] != "tsm1" {
		t.Fatalf("expected free 198.18.2.0/24 + tsm1, got %q %q", res["cidr"], res["tun"])
	}

	// running, in the file, comments intact
	if len(d.liveTailnets()) != 2 {
		t.Fatalf("expected 2 running tailnets, got %d", len(d.liveTailnets()))
	}
	tc := confByName(mustLoad(t, cfgPath), "corp")
	if tc == nil || tc.CIDR != "198.18.2.0/24" || tc.TUN != "tsm1" {
		t.Fatalf("corp not in config as expected: %+v", tc)
	}
	b, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(b), "// comments must survive daemon edits") {
		t.Errorf("comment lost:\n%s", b)
	}
	if _, ok := stub.confs()["corp"]; !ok {
		t.Fatal("stub starter never saw corp")
	}
}

func TestRemoveTailnetEndpointTearsDown(t *testing.T) {
	d, cfgPath, stub := syncDaemon(t)

	rec := doReq(t, d, "DELETE", "/tailnet/dev", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("remove tailnet: %d %s", rec.Code, rec.Body.String())
	}
	if got := len(d.liveTailnets()); got != 0 {
		t.Fatalf("expected 0 running tailnets, got %d", got)
	}
	if confByName(mustLoad(t, cfgPath), "dev") != nil {
		t.Fatal("dev still in config after remove")
	}
	waitStopped(t, stub, "dev")

	// the registry forgot dev: no alias match, no synthetic allocation
	if _, _, ok := d.reg.match("my-server.dev"); ok {
		t.Fatal("registry still matches the removed tailnet's alias")
	}
	if _, ok := d.reg.allocate("dev", "my-server.tailx.ts.net"); ok {
		t.Fatal("registry still allocates for the removed tailnet")
	}
	// the hosts block is empty (markers only)
	hb, _ := os.ReadFile(filepath.Join(filepath.Dir(cfgPath), "hosts"))
	if strings.Contains(string(hb), "198.18.") {
		t.Errorf("hosts block not emptied:\n%s", hb)
	}
}

func TestAddTailnetValidation(t *testing.T) {
	d, _, _ := syncDaemon(t)
	cases := []struct {
		body string
		want string
	}{
		{`{"name": "../etc"}`, "invalid name"},
		{`{"name": "dev"}`, "already exists"},
		{`{"name": "corp", "cidr": "10.0.0.0/24"}`, "outside the synthetic range"},
		{`{"name": "corp", "cidr": "198.18.1.0/24"}`, "overlaps"},
		{`{"name": "corp", "tun": "way-too-long-tun-name"}`, "exceeds 15 chars"},
		{`{"name": "corp", "domain": "NO"}`, "invalid domain"},
		{`{}`, "body must be"},
	}
	for _, c := range cases {
		rec := doReq(t, d, "POST", "/tailnet", c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("POST /tailnet %s: %d %s, want 400 containing %q", c.body, rec.Code, rec.Body.String(), c.want)
		}
	}
	// free-form names are accepted; the domain is pre-populated with a slug
	rec := doReq(t, d, "POST", "/tailnet", `{"name": "MS Infra 2"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"domain":"ms-infra-2"`) {
		t.Errorf("POST /tailnet MS Infra 2: %d %s, want 200 with slugged domain", rec.Code, rec.Body.String())
	}
}

func TestRemoveTailnetUnknownName(t *testing.T) {
	d, _, _ := syncDaemon(t)
	rec := doReq(t, d, "DELETE", "/tailnet/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestReloadSyncsManualEdits(t *testing.T) {
	d, cfgPath, stub := syncDaemon(t)

	// a hand edit: a second tailnet in the file, plus a comment
	edited := strings.Replace(mustRead(t, cfgPath),
		`{ "name": "dev", "cidr": "198.18.1.0/24", "tun": "tsm0" },`,
		`{ "name": "dev", "cidr": "198.18.1.0/24", "tun": "tsm0" },
    { "name": "msinfra", "cidr": "198.18.2.0/24", "tun": "tsm1" }, // hand-edited`, 1)
	if err := os.WriteFile(cfgPath, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, d, "POST", "/reload", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := stub.confs()["msinfra"]; !ok {
		t.Fatal("reload did not start the hand-edited tailnet")
	}
	if len(d.liveTailnets()) != 2 {
		t.Fatalf("expected 2 running tailnets, got %d", len(d.liveTailnets()))
	}

	// and a hand removal: drop dev from the file, reload again
	stripped := strings.Replace(mustRead(t, cfgPath),
		`{ "name": "dev", "cidr": "198.18.1.0/24", "tun": "tsm0" },
    `, "", 1)
	if err := os.WriteFile(cfgPath, []byte(stripped), 0600); err != nil {
		t.Fatal(err)
	}
	if rec := doReq(t, d, "POST", "/reload", ""); rec.Code != http.StatusOK {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	waitStopped(t, stub, "dev")
}

func TestCIDRChangeRestartsTailnet(t *testing.T) {
	d, cfgPath, stub := syncDaemon(t)

	_, err := d.applyConfigUpdate(func(c *Config) error {
		confByName(c, "dev").CIDR = "198.18.5.0/24"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStopped(t, stub, "dev")
	if tc, ok := stub.confs()["dev"]; !ok || tc.CIDR != "198.18.5.0/24" {
		tc, _ := stub.confs()["dev"]
		t.Fatalf("cidr change did not restart dev with the new cidr: %+v", tc)
	}
	if got := confByName(mustLoad(t, cfgPath), "dev").CIDR; got != "198.18.5.0/24" {
		t.Fatalf("config cidr not updated: %s", got)
	}
}

func TestEmptyConfigDaemonServesAndAdds(t *testing.T) {
	// the shipped-from-scratch scenario: zero tailnets, control socket up,
	// first tailnet added through the API
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.jsonc")
	if err := os.WriteFile(cfgPath, []byte(`{
  // nothing configured yet
  "tailnets": []
}`), 0600); err != nil {
		t.Fatal(err)
	}
	reg, err := newRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := newDaemon(nil, reg, cfgPath, filepath.Join(dir, "hosts"))
	d.rt.ctx = context.Background()
	d.rt.baseDir = dir
	d.rt.starter = func(ctx context.Context, tc TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
		_, cancel := context.WithCancel(ctx)
		return &Tailnet{conf: tc, cancel: cancel}, nil
	}
	t.Cleanup(func() {
		for _, tn := range d.liveTailnets() {
			tn.Close()
		}
	})

	if rec := doReq(t, d, "GET", "/config", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /config on empty daemon: %d", rec.Code)
	}
	if rec := doReq(t, d, "GET", "/status", ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /status on empty daemon: %d", rec.Code)
	}

	rec := doReq(t, d, "POST", "/tailnet", `{"name": "dev"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first add on empty daemon: %d %s", rec.Code, rec.Body.String())
	}
	if len(d.liveTailnets()) != 1 {
		t.Fatalf("tailnet not running after add: %d", len(d.liveTailnets()))
	}
}

func TestStartFailureStaysInConfigAndSelfHeals(t *testing.T) {
	d, cfgPath, _ := syncDaemon(t)

	// the starter refuses "corp": the config keeps the entry, sync reports it
	origStarter := d.rt.starter
	d.rt.starter = func(ctx context.Context, tc TailnetConf, reg *registry, mtu uint32, baseDir string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
		if tc.Name == "corp" {
			return nil, errStartRefused
		}
		return origStarter(ctx, tc, reg, mtu, baseDir, onRunning, rs)
	}
	if _, err := d.applyConfigUpdate(func(c *Config) error {
		c.Tailnets = append(c.Tailnets, TailnetConf{Name: "corp", CIDR: "198.18.2.0/24", TUN: "tsm1"})
		return nil
	}); err == nil {
		t.Fatal("expected a start error")
	}
	if confByName(mustLoad(t, cfgPath), "corp") == nil {
		t.Fatal("failed start should keep the config entry (retried later)")
	}
	if len(d.liveTailnets()) != 1 {
		t.Fatalf("corp must not be running, got %d", len(d.liveTailnets()))
	}

	// the starter works again; the next config mutation retries the start
	d.rt.starter = origStarter
	if _, err := d.applyConfigUpdate(func(c *Config) error {
		confByName(c, "dev").Resources = []string{"x"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(d.liveTailnets()) != 2 {
		t.Fatalf("corp was not self-healed by a later config change: %d running", len(d.liveTailnets()))
	}
}

// errStartRefused stands in for a TUN/tsnet start failure in tests.
var errStartRefused = &startError{}

type startError struct{}

func (*startError) Error() string { return "start refused (stub)" }
