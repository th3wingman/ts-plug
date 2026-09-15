package main

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// The embedded UI must serve index.html on "/" alongside the control API, the
// view modules under /views/, and the API patterns must win over the
// catch-all UI route.
func TestUIHandlerServesIndex(t *testing.T) {
	d := newDaemon(nil, nil, "", "")
	srv := httptest.NewServer(d.uiMux())
	defer srv.Close()

	get := func(path string) (int, string, string) {
		t.Helper()
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, res.Header.Get("Content-Type"), string(b)
	}

	code, ct, body := get("/")
	if code != 200 || !strings.Contains(body, "ts-multinet") {
		t.Fatalf("GET / = %d, body %q", code, body)
	}
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / content-type = %q", ct)
	}
	if !strings.Contains(body, `src="./app.js"`) {
		t.Fatalf("index must load the app module, body %q", body)
	}

	// the view modules (and the shared one they import) must be embedded
	for _, tc := range []struct{ path, want string }{
		{"/app.js", "parseRoute"},
		{"/api.js", "ApiError"},
		{"/views/shared.js", "refresh"},
		{"/views/overview.js", "renderOverview"},
		{"/views/tailnet.js", "renderTailnet"},
		{"/views/config.js", "renderConfig"},
		{"/style.css", "--accent"},
	} {
		code, ct, body := get(tc.path)
		if code != 200 || !strings.Contains(body, tc.want) {
			t.Fatalf("GET %s = %d (ct %q), missing %q", tc.path, code, ct, tc.want)
		}
		if strings.Contains(ct, "text/html") {
			t.Fatalf("GET %s served as html (%q)", tc.path, ct)
		}
	}

	// the catch-all must not shadow the API: /config exists and errors
	// cleanly (no config file) rather than serving HTML.
	code, ct, _ = get("/config")
	if code != 500 || strings.Contains(ct, "text/html") {
		t.Fatalf("GET /config = %d %q — UI shadowed the API", code, ct)
	}
}