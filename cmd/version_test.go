package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newVersionAuthConfigServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/config" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVersionReportsServerVersion(t *testing.T) {
	srv := newVersionAuthConfigServer(t, `{"enabled": true, "version": "v0.8.0-alpha.1"}`)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "version")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "server version: v0.8.0-alpha.1") {
		t.Errorf("output = %q, want it to contain server version", out)
	}
}

func TestVersionUnknownWhenServerOmitsVersion(t *testing.T) {
	srv := newVersionAuthConfigServer(t, `{"enabled": false}`)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "version")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "server version: unknown") {
		t.Errorf("output = %q, want it to report unknown server version", out)
	}
}

func TestVersionUnknownWhenServerUnreachable(t *testing.T) {
	out, err := execute(t, "--server", "http://127.0.0.1:1/api/v1/", "version")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "server version: unknown") {
		t.Errorf("output = %q, want it to report unknown server version", out)
	}
}
