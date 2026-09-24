package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovePurgesState(t *testing.T) {
	for _, mode := range []string{"running", "disabled", "failed start", "custom state dir"} {
		t.Run(mode, func(t *testing.T) {
			d, path, _ := syncDaemon(t)
			cfg := mustLoad(t, path)
			tc := &cfg.Tailnets[0]
			tc.AuthKey = "test-placeholder"
			tc.Resources = []string{"peer"}
			if mode == "custom state dir" {
				tc.StateDir = filepath.Join(t.TempDir(), "custom")
			}
			if mode == "disabled" || mode == "failed start" {
				d.stopTailnet(d.tailnetByName("dev"))
				if mode == "disabled" {
					off := false
					tc.Enabled = &off
				}
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			dir := tc.nodeStateDir(d.rt.baseDir)
			if err := os.MkdirAll(filepath.Join(dir, "cache"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{"tailscaled.state", "selections.json", "cache/settings"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte("old state"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			outside := filepath.Join(t.TempDir(), "keep")
			if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "external")); err != nil {
				t.Fatal(err)
			}
			if live := d.tailnetByName("dev"); live != nil {
				live.stateDir = dir
				cancel := live.cancel
				live.cancel = func() {
					if _, err := os.Stat(filepath.Join(dir, "tailscaled.state")); err != nil {
						t.Error("purged before stopping node")
					}
					cancel()
				}
			}
			rec := doReq(t, d, "DELETE", "/tailnet/dev", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("state directory remains: %v", err)
			}
			if b, err := os.ReadFile(outside); err != nil || string(b) != "keep" {
				t.Fatal("purge followed an internal symlink")
			}
			if confByName(mustLoad(t, path), "dev") != nil || d.tailnetByName("dev") != nil {
				t.Fatal("deleted tailnet still configured or running")
			}
			if done, err := d.retryTailnet("dev"); !done || err != nil {
				t.Fatalf("pending retry did not stop: %v %v", done, err)
			}
			if rec := doReq(t, d, "POST", "/tailnet", `{"name":"dev"}`); rec.Code != http.StatusOK {
				t.Fatalf("re-add: %d %s", rec.Code, rec.Body.String())
			}
			after := devConf(t, path)
			if after.AuthKey != "" || len(after.Resources) != 0 || after.StateDir != "" {
				t.Fatal("re-add retained old settings")
			}
		})
	}
}

func TestRemoveRefusesProtectedState(t *testing.T) {
	for _, mode := range []string{"base", "shared", "nested sibling", "nested symlink sibling", "config inside", "symlink root", "live drift"} {
		t.Run(mode, func(t *testing.T) {
			d, path, _ := syncDaemon(t)
			cfg := mustLoad(t, path)
			dir := filepath.Join(d.rt.baseDir, "dev")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			cfg.Tailnets[0].StateDir = dir
			switch mode {
			case "base", "config inside":
				cfg.Tailnets[0].StateDir = filepath.Dir(path)
				if mode == "config inside" {
					d.rt.baseDir = t.TempDir()
				}
			case "shared", "nested sibling", "nested symlink sibling":
				other := dir
				if mode != "shared" {
					other = filepath.Join(dir, "child")
				}
				if mode == "nested symlink sibling" {
					if err := os.Mkdir(other, 0700); err != nil {
						t.Fatal(err)
					}
					alias := filepath.Join(d.rt.baseDir, "alias")
					if err := os.Symlink(other, alias); err != nil {
						t.Fatal(err)
					}
					other = alias
				}
				cfg.Tailnets = append(cfg.Tailnets, TailnetConf{Name: "corp", CIDR: "198.18.2.0/24", TUN: "tsm1", StateDir: other})
			case "symlink root":
				alias := filepath.Join(d.rt.baseDir, "alias")
				if err := os.Symlink(dir, alias); err != nil {
					t.Fatal(err)
				}
				cfg.Tailnets[0].StateDir = alias
			case "live drift":
				d.tailnetByName("dev").stateDir = t.TempDir()
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			rec := doReq(t, d, "DELETE", "/tailnet/dev", "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
			}
			if d.tailnetByName("dev") == nil || !devConf(t, path).enabled() {
				t.Fatal("refused purge changed running state")
			}
		})
	}
}

func TestRemovePurgeFailureIsRetryable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires non-root permissions")
	}
	d, path, _ := syncDaemon(t)
	dir := filepath.Join(d.rt.baseDir, "dev")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tailscaled.state"), []byte("identity"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	rec := doReq(t, d, "DELETE", "/tailnet/dev", "")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "retry delete") {
		t.Fatalf("delete failure: %d %s", rec.Code, rec.Body.String())
	}
	if devConf(t, path).enabled() || d.tailnetByName("dev") != nil {
		t.Fatal("failed purge did not leave disabled config for retry")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if rec := doReq(t, d, "DELETE", "/tailnet/dev", ""); rec.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
	}
}
