package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// contextTestServer records the auth header and last agent path/namespace it
// was asked for, and serves the endpoints agents get/list need.
type contextTestServer struct {
	srv      *httptest.Server
	auth     string
	getPath  string
	listedNS string
}

func newContextTestServer(t *testing.T, namespaces string) *contextTestServer {
	t.Helper()
	c := &contextTestServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":[` + namespaces + `]}`))
		case r.URL.Path == "/api/v1/agents":
			c.auth = r.Header.Get("Authorization")
			c.listedNS = r.URL.Query().Get("namespace")
			_, _ = w.Write([]byte(`{"items":[]}`))
		case strings.HasSuffix(r.URL.Path, "/route-status"):
			// `agents get` asks for this too, to report the external route. It is
			// deliberately not recorded in getPath, which these tests use to check
			// which namespace the detail request went to.
			c.auth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"hasRoute":true}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/agents/"):
			c.auth = r.Header.Get("Authorization")
			c.getPath = r.URL.Path
			_, _ = w.Write([]byte(`{"metadata":{"name":"orders","namespace":"team9"},"spec":{},"status":{},"workloadType":"deployment","readyStatus":"Ready"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func TestAgentsContextOverridesCurrent(t *testing.T) {
	isolateHome(t)

	// Two servers: one for the current context, one for the "other" context.
	current := newContextTestServer(t, `"team1"`)
	other := newContextTestServer(t, `"team9"`)

	// current context "dev" (namespace team1, no token).
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", current.srv.URL+"/api/v1/", "--namespace", "team1"); err != nil {
		t.Fatalf("create-context dev: %v", err)
	}
	// other context "prod" (namespace team9, with a token). Creating it makes
	// it current, so switch back to dev afterward.
	if _, err := execute(t, "config", "create-context",
		"--name", "prod", "--server", other.srv.URL+"/api/v1/", "--namespace", "team9",
		"--bearer-token", "prod-token"); err != nil {
		t.Fatalf("create-context prod: %v", err)
	}
	if _, err := execute(t, "config", "use-context", "dev"); err != nil {
		t.Fatalf("use-context dev: %v", err)
	}

	// agents get with --context prod must hit the prod server, prod namespace,
	// and send prod's token — not the current (dev) context.
	if _, err := execute(t, "agents", "--context", "prod", "get", "orders"); err != nil {
		t.Fatalf("agents get --context prod: %v", err)
	}
	if other.getPath != "/api/v1/agents/team9/orders" {
		t.Errorf("get path = %q, want /api/v1/agents/team9/orders", other.getPath)
	}
	if other.auth != "Bearer prod-token" {
		t.Errorf("auth = %q, want Bearer prod-token", other.auth)
	}
	if current.getPath != "" {
		t.Errorf("current (dev) server should not have been called, got path %q", current.getPath)
	}
}

func TestTopLevelContextFlag(t *testing.T) {
	isolateHome(t)

	// --context is a root-level persistent flag, so it may appear before the
	// subcommand path, not only after the group name.
	current := newContextTestServer(t, `"team1"`)
	other := newContextTestServer(t, `"team9"`)

	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", current.srv.URL+"/api/v1/", "--namespace", "team1"); err != nil {
		t.Fatalf("create-context dev: %v", err)
	}
	if _, err := execute(t, "config", "create-context",
		"--name", "prod", "--server", other.srv.URL+"/api/v1/", "--namespace", "team9",
		"--bearer-token", "prod-token"); err != nil {
		t.Fatalf("create-context prod: %v", err)
	}
	if _, err := execute(t, "config", "use-context", "dev"); err != nil {
		t.Fatalf("use-context dev: %v", err)
	}

	// --context placed at the top level (before "agents") must still route to
	// the prod server, namespace, and token.
	if _, err := execute(t, "--context", "prod", "agents", "get", "orders"); err != nil {
		t.Fatalf("--context prod agents get: %v", err)
	}
	if other.getPath != "/api/v1/agents/team9/orders" {
		t.Errorf("get path = %q, want /api/v1/agents/team9/orders", other.getPath)
	}
	if other.auth != "Bearer prod-token" {
		t.Errorf("auth = %q, want Bearer prod-token", other.auth)
	}
	if current.getPath != "" {
		t.Errorf("current (dev) server should not have been called, got path %q", current.getPath)
	}
}

func TestAgentsListUsesContextNamespace(t *testing.T) {
	isolateHome(t)

	current := newContextTestServer(t, `"team1"`)
	other := newContextTestServer(t, `"team9"`)

	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", current.srv.URL+"/api/v1/", "--namespace", "team1"); err != nil {
		t.Fatalf("create-context dev: %v", err)
	}
	if _, err := execute(t, "config", "create-context",
		"--name", "prod", "--server", other.srv.URL+"/api/v1/", "--namespace", "team9"); err != nil {
		t.Fatalf("create-context prod: %v", err)
	}
	if _, err := execute(t, "config", "use-context", "dev"); err != nil {
		t.Fatalf("use-context dev: %v", err)
	}

	// Default (no --all-namespaces): agents list uses the --context prod
	// namespace (team9) against the prod server.
	if _, err := execute(t, "agents", "--context", "prod", "list"); err != nil {
		t.Fatalf("agents list --context prod: %v", err)
	}
	if other.listedNS != "team9" {
		t.Errorf("listed namespace = %q, want team9 (prod context)", other.listedNS)
	}
	if current.listedNS != "" {
		t.Errorf("current (dev) server should not have been called, listed %q", current.listedNS)
	}
}

func TestAgentsContextUnknownErrors(t *testing.T) {
	isolateHome(t)
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/", "--namespace", "team1"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	if _, err := execute(t, "agents", "--context", "does-not-exist", "get", "orders"); err == nil {
		t.Error("agents --context with an unknown context should error")
	}
}

func TestAgentsNamespaceFlagOverridesContextNamespace(t *testing.T) {
	isolateHome(t)

	current := newContextTestServer(t, `"team1"`)
	other := newContextTestServer(t, `"team9","teamX"`)

	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", current.srv.URL+"/api/v1/", "--namespace", "team1"); err != nil {
		t.Fatalf("create-context dev: %v", err)
	}
	if _, err := execute(t, "config", "create-context",
		"--name", "prod", "--server", other.srv.URL+"/api/v1/", "--namespace", "team9"); err != nil {
		t.Fatalf("create-context prod: %v", err)
	}
	if _, err := execute(t, "config", "use-context", "dev"); err != nil {
		t.Fatalf("use-context dev: %v", err)
	}

	// --context selects the server; --namespace overrides that context's ns.
	if _, err := execute(t, "agents", "--context", "prod", "--namespace", "teamX", "get", "orders"); err != nil {
		t.Fatalf("agents get: %v", err)
	}
	if other.getPath != "/api/v1/agents/teamX/orders" {
		t.Errorf("get path = %q, want /api/v1/agents/teamX/orders", other.getPath)
	}
}
