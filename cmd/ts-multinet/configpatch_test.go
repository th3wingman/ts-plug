package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tailscale/hujson"
)

// testConfig seeds a temp config from the shipped example and returns its path.
func testConfig(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("config.example.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.jsonc")
	if err := os.WriteFile(path, b, 0600); err != nil {
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

// exampleComments must survive every patch — they are what humans read.
var exampleComments = []string{
	"// matches StateDirectory= in the systemd unit",
	"// TUN device name, <= 15 chars (kernel limit)",
	"// optional per tailnet:",
	"// \"resources\": [\"host1\", \"host2\"],     // short names exactly as `peers` shows them",
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

func TestPatchRejectsStructuralChanges(t *testing.T) {
	path := testConfig(t)
	before := mustRead(t, path)

	cases := map[string]func(next *Config){
		"remove tailnet": func(n *Config) { n.Tailnets = n.Tailnets[:len(n.Tailnets)-1] },
		"rename tailnet": func(n *Config) { n.Tailnets[0].Name = "renamed" },
		"change cidr":    func(n *Config) { n.Tailnets[0].CIDR = "198.18.9.0/24" },
		"change tun":     func(n *Config) { n.Tailnets[0].TUN = "tsm9" },
		"add tailnet": func(n *Config) {
			n.Tailnets = append(n.Tailnets, TailnetConf{Name: "x", CIDR: "198.18.2.0/24", TUN: "tsm1"})
		},
	}
	for name, mutate := range cases {
		prev := mustLoad(t, path)
		next, _ := cloneConfig(prev)
		mutate(next)
		if err := patchConfigFile(path, prev, next); err == nil {
			t.Errorf("%s: patch accepted a structural change", name)
		}
	}
	if after := mustRead(t, path); after != before {
		t.Errorf("rejected patch still wrote the file")
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
