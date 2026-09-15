package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tailscale/hujson"
)

// patchFixture is the patch tests' own config: one "example" tailnet with
// the comments the tests assert survive patching. It is deliberately NOT the
// shipped example — that one is empty by design (fresh installs start with
// zero tailnets; see config_test.go).
const patchFixture = `{
  // matches StateDirectory= in the systemd unit
  "state_dir": "/var/lib/ts-multinet",
  "tailnets": [
    {
      "name": "example",
      "cidr": "198.18.1.0/24",
      // TUN device name, <= 15 chars (kernel limit)
      "tun": "tsm0",
      // optional per tailnet:
      // "resources": ["host1", "host2"],     // short names exactly as 'peers' shows them
    },
  ]
}`

// testConfig seeds a temp config from the patch fixture and returns its path.
func testConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.jsonc")
	if err := os.WriteFile(path, []byte(patchFixture), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustLoad(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fixture comments must survive every patch — they are what humans read.
var exampleComments = []string{
	"// matches StateDirectory= in the systemd unit",
	"// TUN device name, <= 15 chars (kernel limit)",
	"// optional per tailnet:",
	"// \"resources\": [\"host1\", \"host2\"],     // short names exactly as 'peers' shows them",
}

func assertComments(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range exampleComments {
		if !strings.Contains(string(b), want) {
			t.Errorf("comment lost: %q\nfile:\n%s", want, b)
		}
	}
}

func TestPatchRoundTripPreservesComments(t *testing.T) {
	path := testConfig(t)
	prev := mustLoad(t, path)
	next, err := cloneConfig(prev)
	if err != nil {
		t.Fatal(err)
	}
	tc := confByName(next, "example")
	tc.Domain = "lab"
	tc.AllowAll = true
	tc.Resources = []string{"host1", "host2"}

	if err := patchConfigFile(path, prev, next); err != nil {
		t.Fatal(err)
	}
	assertComments(t, path)

	got := confByName(mustLoad(t, path), "example")
	if got.Domain != "lab" || !got.AllowAll || !slices.Equal(got.Resources, []string{"host1", "host2"}) {
		t.Fatalf("patched config wrong: %+v", got)
	}
}

// auth_key round-trips through the patcher like any mutable field: set lands
// in the file, clearing removes the member, comments survive.
func TestPatchAuthKey(t *testing.T) {
	path := testConfig(t)
	prev := mustLoad(t, path)
	next, err := cloneConfig(prev)
	if err != nil {
		t.Fatal(err)
	}
	confByName(next, "example").AuthKey = "tskey-auth-abc123"
	if err := patchConfigFile(path, prev, next); err != nil {
		t.Fatal(err)
	}
	assertComments(t, path)
	if got := confByName(mustLoad(t, path), "example"); got.AuthKey != "tskey-auth-abc123" {
		t.Fatalf("auth_key not patched in: %+v", got)
	}

	cur := mustLoad(t, path)
	reset, _ := cloneConfig(cur)
	confByName(reset, "example").AuthKey = ""
	if err := patchConfigFile(path, cur, reset); err != nil {
		t.Fatal(err)
	}
	if got := confByName(mustLoad(t, path), "example"); got.AuthKey != "" {
		t.Fatalf("auth_key not cleared: %+v", got)
	}
}

func TestPatchForgetReverses(t *testing.T) {
	path := testConfig(t)
	prev := mustLoad(t, path)
	next, _ := cloneConfig(prev)
	confByName(next, "example").Resources = []string{"host1"}
	if err := patchConfigFile(path, prev, next); err != nil {
		t.Fatal(err)
	}

	// forget everything, drop allow_all and the domain override
	cur := mustLoad(t, path)
	reset, _ := cloneConfig(cur)
	tc := confByName(reset, "example")
	tc.Resources = nil
	tc.AllowAll = false
	tc.Domain = ""
	if err := patchConfigFile(path, cur, reset); err != nil {
		t.Fatal(err)
	}
	assertComments(t, path)

	got := confByName(mustLoad(t, path), "example")
	if len(got.Resources) != 0 || got.AllowAll || got.Domain != "" {
		t.Fatalf("reversal incomplete: %+v", got)
	}
	// The example documents "domain" in a comment, so check the live members,
	// not the raw text: standardize (strip comments) before looking.
	if std, err := hujson.Standardize([]byte(mustRead(t, path))); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(std), `"domain"`) {
		t.Errorf("removed domain member survived:\n%s", mustRead(t, path))
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPatchInsertsMissingMembers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	src := `{
	  // the only comment that must survive
	  "tailnets": [
	    { "name": "dev", "cidr": "198.18.1.0/24", "tun": "tsm0" },
	  ]
	}`
	if err := os.WriteFile(path, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	prev := mustLoad(t, path)
	next, _ := cloneConfig(prev)
	tc := confByName(next, "dev")
	tc.Domain = "dev.corp"
	tc.AllowAll = true
	tc.Resources = []string{"host1"}

	if err := patchConfigFile(path, prev, next); err != nil {
		t.Fatal(err)
	}
	out := mustRead(t, path)
	if !strings.Contains(out, "// the only comment that must survive") {
		t.Errorf("comment lost:\n%s", out)
	}
	got := confByName(mustLoad(t, path), "dev")
	if got.Domain != "dev.corp" || !got.AllowAll || !slices.Equal(got.Resources, []string{"host1"}) {
		t.Fatalf("inserted members wrong: %+v", got)
	}
}

func TestPatchAddRewriteRemoveTailnet(t *testing.T) {
	path := testConfig(t)

	// add: bare members, comments elsewhere untouched
	prev := mustLoad(t, path)
	next, _ := cloneConfig(prev)
	corp := TailnetConf{Name: "corp", CIDR: "198.18.2.0/24", TUN: "tsm1", Domain: "hq", AllowAll: true, Resources: []string{"host1"}}
	next.Tailnets = append(next.Tailnets, corp)
	if err := patchConfigFile(path, prev, next); err != nil {
		t.Fatal(err)
	}
	assertComments(t, path)
	got := confByName(mustLoad(t, path), "corp")
	if got == nil || got.CIDR != corp.CIDR || got.TUN != corp.TUN || got.Domain != "hq" || !got.AllowAll || !slices.Equal(got.Resources, []string{"host1"}) {
		t.Fatalf("added tailnet round-trip wrong: %+v", got)
	}

	// rewrite: a cidr/tun change rewrites the element wholesale
	cur := mustLoad(t, path)
	edited, _ := cloneConfig(cur)
	e := confByName(edited, "corp")
	e.CIDR = "198.18.3.0/24"
	e.TUN = "tsm9"
	e.Domain = "" // unset optionals drop out of the rewritten element
	if err := patchConfigFile(path, cur, edited); err != nil {
		t.Fatal(err)
	}
	assertComments(t, path)
	got = confByName(mustLoad(t, path), "corp")
	if got.CIDR != "198.18.3.0/24" || got.TUN != "tsm9" || got.Domain != "" || !got.AllowAll {
		t.Fatalf("rewritten tailnet wrong: %+v", got)
	}

	// remove: the element is gone, the rest untouched
	cur = mustLoad(t, path)
	drop, _ := cloneConfig(cur)
	drop.Tailnets = slices.DeleteFunc(drop.Tailnets, func(tc TailnetConf) bool { return tc.Name == "corp" })
	if err := patchConfigFile(path, cur, drop); err != nil {
		t.Fatal(err)
	}
	assertComments(t, path)
	if confByName(mustLoad(t, path), "corp") != nil {
		t.Fatal("corp still in config after remove")
	}
}

func TestPatchRemoveLastTailnetLeavesValidEmptyConfig(t *testing.T) {
	// The empty-config scenario: deleting the final tailnet must leave a
	// file that still parses (with zero tailnets) and keeps its comments.
	path := testConfig(t)
	prev := mustLoad(t, path)
	next, _ := cloneConfig(prev)
	next.Tailnets = nil
	if err := patchConfigFile(path, prev, next); err != nil {
		t.Fatal(err)
	}
	if cfg := mustLoad(t, path); len(cfg.Tailnets) != 0 {
		t.Fatalf("expected zero tailnets, got %+v", cfg.Tailnets)
	}
	if !strings.Contains(mustRead(t, path), "// matches StateDirectory= in the systemd unit") {
		t.Errorf("top-level comment lost:\n%s", mustRead(t, path))
	}
}

func TestPatchRenameLandsAsRemoveAndAdd(t *testing.T) {
	// A rename is indistinguishable from remove+add, so that is what it is.
	path := testConfig(t)
	prev := mustLoad(t, path)
	next, _ := cloneConfig(prev)
	next.Tailnets[0].Name = "renamed"
	if err := patchConfigFile(path, prev, next); err != nil {
		t.Fatal(err)
	}
	got := mustLoad(t, path)
	if len(got.Tailnets) != 1 || got.Tailnets[0].Name != "renamed" {
		t.Fatalf("rename did not land as remove+add: %+v", got.Tailnets)
	}
}

func TestPatchUnknownTailnetInFile(t *testing.T) {
	path := testConfig(t)
	prev := mustLoad(t, path)
	next, _ := cloneConfig(prev)
	confByName(next, "example").Resources = []string{"host1"}

	// the file no longer contains the tailnet the patch targets
	sabotaged := strings.Replace(mustRead(t, path), `"example"`, `"renamed"`, 1)
	if err := os.WriteFile(path, []byte(sabotaged), 0600); err != nil {
		t.Fatal(err)
	}
	if err := patchConfigFile(path, prev, next); err == nil {
		t.Fatal("patched against a tailnet missing from the file")
	}
}

func TestPatchPreservesFileMode(t *testing.T) {
	path := testConfig(t)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	prev := mustLoad(t, path)
	next, _ := cloneConfig(prev)
	confByName(next, "example").Domain = "lab"
	if err := patchConfigFile(path, prev, next); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0644 {
		t.Errorf("mode changed: %v", fi.Mode().Perm())
	}
}

func TestValidDomain(t *testing.T) {
	cases := map[string]bool{
		"skynet":                             true,
		"dev.corp":                           true,
		"a":                                  true,
		"b-" + strings.Repeat("x", 60) + "y": true, // 63-char label
		"":                                   false,
		"Bad":                                false, // uppercase
		"-x":                                 false,
		"x-":                                 false,
		"a..b":                               false,
		".a":                                 false,
		"a.":                                 false,
		"a_b":                                false,
	}
	for domain, want := range cases {
		if got := validDomain(domain); got != want {
			t.Errorf("validDomain(%q) = %v, want %v", domain, got, want)
		}
	}
}
