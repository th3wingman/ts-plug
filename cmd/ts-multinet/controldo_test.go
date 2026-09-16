package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// controlDo must surface a plain-text non-2xx reply (e.g. a stale daemon's
// "404 page not found") as a readable error, not a json unmarshal panic line.
func TestControlDoPlainTextError(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	var out struct {
		OK string `json:"ok"`
	}
	err = controlDo(sock, http.MethodPost, "/tailnet/x/hostname", nil, &out)
	if err == nil {
		t.Fatal("expected an error for the plain-text 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error should name the status, got: %v", err)
	}
	if strings.Contains(err.Error(), "unmarshal") {
		t.Fatalf("error must not be a json decode message, got: %v", err)
	}
}
