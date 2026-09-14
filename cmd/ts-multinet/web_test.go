package main

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded UI must serve index.html on "/" alongside the control API,
// and the API patterns must win over the catch-all UI route.
func TestUIHandlerServesIndex(t *testing.T) {
	d := newDaemon(nil, nil, "", "")
	srv := httptest.NewServer(d.uiMux())
	defer srv.Close()

	res, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || !strings.Contains(string(b), "ts-multinet") {
		t.Fatalf("GET / = %d, body %q", res.StatusCode, string(b))
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / content-type = %q", ct)
	}

	// the catch-all must not shadow the API: /config exists and errors
	// cleanly (no config file) rather than serving HTML.
	res2, err := srv.Client().Get(srv.URL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != 500 || strings.Contains(res2.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /config = %d %q — UI shadowed the API", res2.StatusCode, res2.Header.Get("Content-Type"))
	}
}
