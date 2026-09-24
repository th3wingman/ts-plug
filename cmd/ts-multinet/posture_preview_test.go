package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"tailscale.com/client/local"
)

func TestPosturePreview(t *testing.T) {
	for _, mode := range []string{"off", "on", "drift", "disabled", "not started", "prefs error", "partial collection", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			d, path := stubDaemon(t)
			cfg := mustLoad(t, path)
			cfg.Tailnets[0].ReportPosture = mode == "on" || mode == "drift"
			if mode == "disabled" {
				off := false
				cfg.Tailnets[0].Enabled = &off
			}
			if err := patchConfigFile(path, mustLoad(t, path), cfg); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var methods []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				methods = append(methods, r.Method+" "+r.URL.Path)
				if mode == "prefs error" {
					http.Error(w, "node unavailable", http.StatusServiceUnavailable)
					return
				}
				// Unrelated preferences must not leak through the preview.
				json.NewEncoder(w).Encode(map[string]any{
					"PostureChecking": mode == "on",
					"Hostname":        "not-part-of-preview",
					"OperatorUser":    "not-part-of-preview",
				})
			}))
			defer srv.Close()
			d.tailnets[0].lc = &local.Client{OmitAuth: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
			}}
			d.tailnets[0].state = "Running"
			if mode == "disabled" || mode == "not started" {
				d.tailnets = nil
			}
			calls := 0
			d.postureIdentity = func() postureIdentityJSON {
				calls++
				out := postureIdentityJSON{SerialNumbers: []string{"sample-serial"}, MACAddresses: []string{"02:00:00:00:00:01"}}
				if mode == "partial collection" {
					out.SerialNumbers = nil
					out.SerialError = "SMBIOS permission denied"
				}
				return out
			}
			// Ordinary polling must never collect hardware identifiers.
			doReq(t, d, "GET", "/config", "")
			if calls != 0 {
				t.Fatal("config poll collected identifiers")
			}
			name := "dev"
			if mode == "unknown" {
				name = "missing"
			}
			rec := doReq(t, d, "GET", "/tailnet/"+name+"/posture-preview", "")
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("sensitive response can be cached")
			}
			if mode == "unknown" {
				if rec.Code != http.StatusNotFound || calls != 0 || len(methods) != 0 {
					t.Fatal("unknown tailnet must not collect or query node")
				}
				return
			}
			if rec.Code != http.StatusOK || calls != 1 {
				t.Fatalf("preview = %d %s; collector calls=%d", rec.Code, rec.Body.String(), calls)
			}
			var out posturePreviewJSON
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Configured != cfg.Tailnets[0].ReportPosture || out.CollectedAt == "" {
				t.Fatal("preview missing configured state or timestamp")
			}
			if mode == "disabled" || mode == "not started" {
				if out.Applied != nil || out.NodeState != mode || len(methods) != 0 {
					t.Fatal("stopped node must not claim an applied preference or query LocalAPI")
				}
			} else {
				if !reflect.DeepEqual(methods, []string{"GET /localapi/v0/prefs"}) {
					t.Fatalf("preview must only read prefs: %v", methods)
				}
				if mode == "prefs error" {
					if out.Applied != nil || out.PrefsError == "" {
						t.Fatal("preference error presented as a known state")
					}
				} else if out.Applied == nil || *out.Applied != (mode == "on") {
					t.Fatal("preview did not read actual node preference")
				}
			}
			if len(out.MACAddresses) != 1 || (mode == "partial collection" && out.SerialError == "") {
				t.Fatal("partial collector result lost")
			}
			var wire map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			allowed := map[string]bool{"serial_numbers": true, "mac_addresses": true, "serial_error": true, "mac_error": true, "collected_at": true, "configured": true, "applied": true, "node_state": true, "prefs_error": true}
			for key := range wire {
				if !allowed[key] {
					t.Errorf("unexpected disclosed field %s", key)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("preview changed config or consent")
			}
		})
	}
}
