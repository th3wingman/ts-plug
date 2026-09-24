package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResetIdentity(t *testing.T) {
	for _, mode := range []string{"running", "disabled", "custom directory", "missing state"} {
		t.Run(mode, func(t *testing.T) {
			d, path, _ := syncDaemon(t)
			cfg := mustLoad(t, path)
			tc := &cfg.Tailnets[0]
			tc.AuthKey = "test-enrollment-placeholder"
			tc.Resources = []string{"my-server"}
			if mode == "custom directory" {
				tc.StateDir = t.TempDir()
			}
			if mode == "disabled" {
				off := false
				tc.Enabled = &off
				d.stopTailnet(d.tailnetByName("dev"))
			}
			// state_dir is manual-only, so seed the fixture as a manual edit.
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			dir := tc.nodeStateDir(d.rt.baseDir)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(dir, "tailscaled.state")
			if mode != "missing state" {
				if err := os.WriteFile(store, []byte("old local identity"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			pins := filepath.Join(dir, "selections.json")
			if err := os.WriteFile(pins, []byte("keep peer identity pins"), 0600); err != nil {
				t.Fatal(err)
			}
			if live := d.tailnetByName("dev"); live != nil {
				live.stateDir = dir
				cancel := live.cancel
				live.cancel = func() {
					if mode != "missing state" {
						if _, err := os.Stat(store); err != nil {
							t.Error("identity removed before stopping the node")
						}
					}
					cancel()
				}
			}
			rec := doReq(t, d, "POST", "/tailnet/dev/reset-identity", `{"confirm":"dev"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
			}
			if _, err := os.Stat(store); !os.IsNotExist(err) {
				t.Fatalf("identity store remains: %v", err)
			}
			if b, err := os.ReadFile(pins); err != nil || string(b) != "keep peer identity pins" {
				t.Fatalf("peer pins changed: %q %v", b, err)
			}
			after := devConf(t, path)
			if after.enabled() || after.AuthKey != "" || strings.Join(after.Resources, ",") != "my-server" {
				t.Fatal("must disable, clear enrollment key, and keep selections")
			}
			if d.tailnetByName("dev") != nil {
				t.Fatal("reset tailnet still running")
			}
			if done, err := d.retryTailnet("dev"); !done || err != nil || d.tailnetByName("dev") != nil {
				t.Fatalf("pending TUN retry resurrected reset tailnet: %v %v", done, err)
			}
			d.rt.starter = func(ctx context.Context, tc TailnetConf, reg *registry, mtu uint32, base string, onRunning func(), rs *resolvedSync) (*Tailnet, error) {
				if tc.AuthKey != "" {
					t.Error("stale enrollment key reused")
				}
				if _, err := os.Stat(store); !os.IsNotExist(err) {
					t.Error("old identity reused on enable")
				}
				return &Tailnet{conf: tc, state: "NeedsLogin"}, nil
			}
			if rec := doReq(t, d, "POST", "/tailnet/dev/enabled", `{"on":true}`); rec.Code != http.StatusOK {
				t.Fatalf("enable = %d %s", rec.Code, rec.Body.String())
			}
			if s, _ := d.tailnetByName("dev").status(); s != "NeedsLogin" {
				t.Fatal("new enrollment not required")
			}
		})
	}
}

func TestResetIdentityRefusesUnsafeRequests(t *testing.T) {
	for _, mode := range []string{"unconfirmed", "wrong name", "form post", "unknown", "locked", "symlink store", "directory store", "shared directory", "state directory drift", "unwritable config"} {
		t.Run(mode, func(t *testing.T) {
			d, path, _ := syncDaemon(t)
			dir := filepath.Join(d.rt.baseDir, "dev")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(dir, "tailscaled.state")
			if err := os.WriteFile(store, []byte("keep identity"), 0600); err != nil {
				t.Fatal(err)
			}
			body, name, want := `{"confirm":"dev"}`, "dev", http.StatusBadRequest
			cfg := mustLoad(t, path)
			switch mode {
			case "unconfirmed":
				body = `{}`
			case "wrong name":
				body = `{"confirm":"corp"}`
			case "unknown":
				name, body, want = "missing", `{"confirm":"missing"}`, http.StatusNotFound
			case "locked":
				cfg.Tailnets[0].Locked = []string{"my-server"}
				cfg.Tailnets[0].Resources = []string{"my-server"}
				want = http.StatusConflict
			case "symlink store", "directory store":
				if err := os.Rename(store, store+"-target"); err != nil {
					t.Fatal(err)
				}
				var err error
				if mode == "symlink store" {
					err = os.Symlink(store+"-target", store)
				} else {
					err = os.Mkdir(store, 0700)
				}
				if err != nil {
					t.Fatal(err)
				}
				want = http.StatusConflict
			case "shared directory":
				alias := filepath.Join(d.rt.baseDir, "alias")
				if err := os.Symlink(dir, alias); err != nil {
					t.Fatal(err)
				}
				cfg.Tailnets = append(cfg.Tailnets, TailnetConf{Name: "corp", CIDR: "198.18.2.0/24", TUN: "tsm1", StateDir: alias})
				want = http.StatusConflict
			case "state directory drift":
				d.tailnetByName("dev").stateDir = dir
				cfg.Tailnets[0].StateDir = t.TempDir()
				want = http.StatusConflict
			case "unwritable config":
				want = http.StatusInternalServerError
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "unwritable config" {
				if os.Geteuid() == 0 {
					t.Skip("permission test requires non-root")
				}
				parent := filepath.Dir(path)
				if err := os.Chmod(parent, 0500); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(parent, 0700)
			}
			req := httptest.NewRequest("POST", "/tailnet/"+name+"/reset-identity", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if mode == "form post" {
				req.Header.Set("Content-Type", "text/plain")
			}
			rec := httptest.NewRecorder()
			d.controlMux().ServeHTTP(rec, req)
			if rec.Code != want {
				t.Fatalf("reset = %d %s, want %d", rec.Code, rec.Body.String(), want)
			}
			if _, err := os.Lstat(store); err != nil || d.tailnetByName("dev") == nil || !devConf(t, path).enabled() {
				t.Fatal("refused reset changed state or stopped the node")
			}
		})
	}
}

func TestResetIdentityRemovalFailureStaysDisabled(t *testing.T) {
	d, path, _ := syncDaemon(t)
	dir := filepath.Join(d.rt.baseDir, "dev")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "tailscaled.state")
	if err := os.WriteFile(store, []byte("identity"), 0600); err != nil {
		t.Fatal(err)
	}
	live := d.tailnetByName("dev")
	cancel := live.cancel
	live.cancel = func() {
		cancel()
		// Simulate a filesystem failure after shutdown, before removal.
		if err := os.Rename(store, store+"-saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(store, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(store, "block"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rec := doReq(t, d, "POST", "/tailnet/dev/reset-identity", `{"confirm":"dev"}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "leave disabled and retry") {
		t.Fatalf("reset failure = %d %s", rec.Code, rec.Body.String())
	}
	if devConf(t, path).enabled() || d.tailnetByName("dev") != nil {
		t.Fatal("failed reset re-enabled the node")
	}
}
