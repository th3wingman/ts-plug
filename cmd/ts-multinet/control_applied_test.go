package main

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// /applied must report no drift right after a sync, then drift once the file is
// touched — that is the UI's "manual reload needed" signal.
func TestAppliedTracksConfigDrift(t *testing.T) {
	d, cfgPath := stubDaemon(t)

	// no apply yet: nothing to compare against, so it must not cry drift
	rec := doReq(t, d, "GET", "/applied", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /applied = %d %s", rec.Code, rec.Body.String())
	}
	var got appliedJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Dirty {
		t.Fatal("dirty before any apply")
	}

	// a sync records the file's mtime: immediately clean
	if msg := d.reload(); msg == "" {
		t.Fatal("reload returned no message")
	}
	rec = doReq(t, d, "GET", "/applied", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Dirty || got.AppliedAt == "" {
		t.Fatalf("after reload: %+v", got)
	}

	// a manual edit after the apply is drift until the next reload
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(cfgPath, future, future); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, d, "GET", "/applied", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Dirty {
		t.Fatal("manual edit not reported as drift")
	}
}

// /clear empties resources and turns allow-all off in one write.
func TestClearSelections(t *testing.T) {
	d, cfgPath := stubDaemon(t)

	if rec := doReq(t, d, "POST", "/tailnet/dev/select", `{"peer":"my-server"}`); rec.Code != http.StatusOK {
		t.Fatalf("select = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, d, "POST", "/tailnet/dev/allow-all", `{"on":true}`); rec.Code != http.StatusOK {
		t.Fatalf("allow-all = %d %s", rec.Code, rec.Body.String())
	}

	rec := doReq(t, d, "POST", "/tailnet/dev/clear", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body.String())
	}
	tc := devConf(t, cfgPath)
	if len(tc.Resources) != 0 || tc.AllowAll {
		t.Fatalf("clear left selections behind: resources=%v allow_all=%v", tc.Resources, tc.AllowAll)
	}
}
