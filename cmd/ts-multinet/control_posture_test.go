package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"tailscale.com/client/local"
	"tailscale.com/feature"
	"tailscale.com/ipn"
)

func TestReportPostureOptIn(t *testing.T) {
	d, path, stub := syncDaemon(t)
	d.peerShorts = func(context.Context, *Tailnet) ([]string, error) { return nil, nil }
	if devConf(t, path).ReportPosture {
		t.Fatal("posture reporting must default off")
	}
	if rec := doReq(t, d, "POST", "/tailnet", `{"name":"corp"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	sibling := d.tailnetByName("corp")
	original := d.tailnetByName("dev")
	for _, body := range []string{`{}`, `{"on":null}`, `{"on":"true"}`, `broken`} {
		if rec := doReq(t, d, "POST", "/tailnet/dev/report-posture", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad request %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := doReq(t, d, "POST", "/tailnet/missing/report-posture", `{"on":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown tailnet: %d %s", rec.Code, rec.Body.String())
	}
	if d.tailnetByName("dev") != original {
		t.Fatal("invalid request restarted tailnet")
	}

	for _, on := range []bool{true, false} {
		body, _ := json.Marshal(map[string]bool{"on": on})
		rec := doReq(t, d, "POST", "/tailnet/dev/report-posture", string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("posture toggle: %d %s", rec.Code, rec.Body.String())
		}
		waitStopped(t, stub, "dev")
		if devConf(t, path).ReportPosture != on || stub.confs()["dev"].ReportPosture != on {
			t.Fatal("consent did not reach config and restarted node")
		}
		if d.tailnetByName("corp") != sibling || confByName(mustLoad(t, path), "corp").ReportPosture {
			t.Fatal("toggle affected another tailnet")
		}
		// No-op updates must not restart an already-correct node.
		live := d.tailnetByName("dev")
		if rec := doReq(t, d, "POST", "/tailnet/dev/report-posture", string(body)); rec.Code != http.StatusOK {
			t.Fatal(rec.Body.String())
		}
		if d.tailnetByName("dev") != live {
			t.Fatal("unchanged consent restarted node")
		}
	}

	if rec := doReq(t, d, "POST", "/tailnet/dev/enabled", `{"on":false}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	waitStopped(t, stub, "dev")
	if rec := doReq(t, d, "POST", "/tailnet/dev/report-posture", `{"on":true}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if d.tailnetByName("dev") != nil || !devConf(t, path).ReportPosture {
		t.Fatal("consent update must not enable a disabled tailnet")
	}
	// Structural rewrites and manual reload must keep this optional field.
	if rec := doReq(t, d, "POST", "/tailnet/dev/tun", `{"tun":"tsm9"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if !devConf(t, path).ReportPosture {
		t.Fatal("structural rewrite dropped consent")
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/enabled", `{"on":true}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if !stub.confs()["dev"].ReportPosture {
		t.Fatal("enable did not carry saved consent")
	}
	cfg := mustLoad(t, path)
	next, _ := cloneConfig(cfg)
	confByName(next, "dev").ReportPosture = false
	if err := patchConfigFile(path, cfg, next); err != nil {
		t.Fatal(err)
	}
	d.reload()
	waitStopped(t, stub, "dev")
	if stub.confs()["dev"].ReportPosture {
		t.Fatal("reload did not revoke consent")
	}
}

// Check the exact masked preference sent through the server-provided local
// client. No connection to the system tailscaled or real control plane.
func TestSetReportPosture(t *testing.T) {
	if !feature.IsRegistered("posture") {
		t.Fatal("upstream posture feature is not linked")
	}
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[on], func(t *testing.T) {
			calls := 0
			fail := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "PATCH" || r.URL.Path != "/localapi/v0/prefs" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var got, want map[string]any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				// Compare the wire representation: upstream opt.Bool normalizes
				// an empty value to "unset" when decoding into a Prefs struct.
				data, err := json.Marshal(ipn.MaskedPrefs{Prefs: ipn.Prefs{PostureChecking: on}, PostureCheckingSet: true})
				if err != nil {
					t.Error(err)
					return
				}
				if err := json.Unmarshal(data, &want); err != nil {
					t.Error(err)
					return
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("prefs = %+v, want %+v", got, want)
				}
				if fail {
					http.Error(w, "preference rejected", http.StatusInternalServerError)
					return
				}
				json.NewEncoder(w).Encode(ipn.Prefs{PostureChecking: on})
			}))
			defer srv.Close()
			lc := &local.Client{OmitAuth: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
			}}
			if err := setReportPosture(context.Background(), lc, on); err != nil {
				t.Fatal(err)
			}
			fail = true
			if err := setReportPosture(context.Background(), lc, on); err == nil {
				t.Fatal("failed preference update swallowed")
			}
			if calls != 2 {
				t.Fatalf("got %d local API requests, want 2", calls)
			}
		})
	}
}
