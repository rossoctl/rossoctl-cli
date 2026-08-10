package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
	"github.com/rossoctl/rossoctl-cli/internal/config"
)

func TestLoginSetsTokenOnCurrentContext(t *testing.T) {
	path := isolateHome(t)

	// Establish a known current context.
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	out, err := execute(t, "login", "--token", "sekret")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(out, "dev") {
		t.Errorf("login output should name the context:\n%s", out)
	}

	// The token must be persisted on the current context.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cur, ok := cfg.Current()
	if !ok {
		t.Fatal("no current context after login")
	}
	if cur.Name != "dev" {
		t.Errorf("current context = %q, want dev", cur.Name)
	}
	if cur.BearerToken != "sekret" {
		t.Errorf("token = %q, want sekret", cur.BearerToken)
	}
}

// deviceLoginServer serves both the rossoctl /auth/config endpoint and the
// Keycloak device+token endpoints. The token endpoint returns
// authorization_pending once, then the access token, exercising the poll loop.
func deviceLoginServer(t *testing.T, enabled bool) *httptest.Server {
	t.Helper()
	var tokenCalls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/namespaces":
			// login lists namespaces after obtaining a token to seed a blank
			// context namespace.
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case r.URL.Path == "/api/v1/auth/config":
			if !enabled {
				_, _ = w.Write([]byte(`{"enabled": false}`))
				return
			}
			// keycloak_url points back at this same server.
			_, _ = w.Write([]byte(`{"enabled":true,"keycloak_url":"` + srv.URL +
				`","realm":"rossoctl","client_id":"rossoctl-ui","redirect_uri":null}`))
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/auth/device"):
			_, _ = w.Write([]byte(`{"device_code":"DEV","user_code":"WDJB-MJHT",` +
				`"verification_uri":"` + srv.URL + `/device","expires_in":600,"interval":1}`))
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			tokenCalls++
			if tokenCalls < 2 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"DEVICE-TOKEN"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginDeviceFlow(t *testing.T) {
	path := isolateHome(t)
	srv := deviceLoginServer(t, true)

	// Point the current context at the mock server (also serves Keycloak).
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	// No --token: runs the device flow and saves the resulting token.
	if _, err := execute(t, "login"); err != nil {
		t.Fatalf("login (device flow): %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cur, _ := cfg.Current()
	if cur.BearerToken != "DEVICE-TOKEN" {
		t.Errorf("token = %q, want DEVICE-TOKEN", cur.BearerToken)
	}
	// A blank namespace is seeded from the server's first namespace after login.
	if cur.Namespace != "team1" {
		t.Errorf("namespace = %q, want team1 (seeded after login)", cur.Namespace)
	}
}

func TestLoginDeviceFlowPrintsCode(t *testing.T) {
	isolateHome(t)
	srv := deviceLoginServer(t, true)
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	// The verification URL and user code are shown on stderr; stdout stays for
	// the final confirmation only.
	_, stderr, err := executeSplit(t, "login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(stderr, "WDJB-MJHT") {
		t.Errorf("stderr missing user code:\n%s", stderr)
	}
	if !strings.Contains(stderr, "/device") {
		t.Errorf("stderr missing verification URL:\n%s", stderr)
	}
}

func TestLoginDeviceFlowAuthDisabled(t *testing.T) {
	isolateHome(t)
	srv := deviceLoginServer(t, false) // enabled=false
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if _, err := execute(t, "login"); err == nil {
		t.Error("login should error when server auth is disabled and no --token given")
	}
}

// TestDeviceLoginDoesNotCheckEnabled verifies the enablement check lives in the
// caller rather than in deviceLogin: called directly with a disabled config,
// deviceLogin proceeds to the Keycloak flow instead of refusing.
//
// It also pins that deviceLogin does not re-fetch /auth/config — the config it
// is handed is the one it uses. The test server fails the test if /auth/config
// is requested at all.
func TestDeviceLoginDoesNotCheckEnabled(t *testing.T) {
	isolateHome(t)

	var authConfigCalls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/auth/config":
			authConfigCalls++
			_, _ = w.Write([]byte(`{"enabled": false}`))
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/auth/device"):
			_, _ = w.Write([]byte(`{"device_code":"DEV","user_code":"CODE-1234",` +
				`"verification_uri":"` + srv.URL + `/device","expires_in":600,"interval":1}`))
		case strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"DIRECT-TOKEN"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	// Enabled is false, yet deviceLogin should still run the flow: judging that
	// is the caller's job now.
	disabled := &apiclient.AuthConfig{
		Enabled:     false,
		KeycloakURL: new(srv.URL),
		Realm:       new("rossoctl"),
		ClientID:    new("rossoctl-ui"),
	}

	cmd, _, err := rootCmd.Find([]string{"login"})
	if err != nil {
		t.Fatalf("login not found: %v", err)
	}
	cmd.SetContext(context.Background())

	token, err := deviceLogin(cmd, disabled)
	if err != nil {
		t.Fatalf("deviceLogin should run the flow regardless of Enabled: %v", err)
	}
	if token != "DIRECT-TOKEN" {
		t.Errorf("token = %q, want DIRECT-TOKEN", token)
	}
	if authConfigCalls != 0 {
		t.Errorf("deviceLogin fetched /auth/config %d times; it should use the config it was given",
			authConfigCalls)
	}
}

func TestLoginSeedsContextWhenMissing(t *testing.T) {
	path := isolateHome(t)

	// No context yet: login should seed one (from the default server) and set
	// the token on it.
	if _, err := execute(t, "login", "--token", "tok"); err != nil {
		t.Fatalf("login: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cur, ok := cfg.Current()
	if !ok {
		t.Fatal("login did not create a current context")
	}
	if cur.Server != defaultServer {
		t.Errorf("seeded server = %q, want %q", cur.Server, defaultServer)
	}
	// The seeded context is named after the server's hostname, not the URI.
	if cur.Name != "rossoctl-ui.localtest.me" {
		t.Errorf("seeded context name = %q, want the hostname rossoctl-ui.localtest.me", cur.Name)
	}
	if cur.BearerToken != "tok" {
		t.Errorf("token = %q, want tok", cur.BearerToken)
	}
}

func TestLoginServerCreatesHostnameContext(t *testing.T) {
	path := isolateHome(t)

	// A pre-existing current context, unrelated to the --server host.
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/", "--namespace", "team1"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	// login --server for a NEW host must create a context named after that
	// host, set the token there, and make it current.
	if _, err := execute(t, "login", "--server", "http://newhost:8080/api/v1/", "--token", "tok"); err != nil {
		t.Fatalf("login --server: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ctx, ok := cfg.Get("newhost")
	if !ok {
		t.Fatal("expected a context named after the server hostname 'newhost'")
	}
	if ctx.Server != "http://newhost:8080/api/v1/" {
		t.Errorf("server = %q, want the full URI", ctx.Server)
	}
	if ctx.BearerToken != "tok" {
		t.Errorf("token = %q, want tok", ctx.BearerToken)
	}
	if cfg.CurrentContext != "newhost" {
		t.Errorf("current context = %q, want newhost", cfg.CurrentContext)
	}
	// The pre-existing dev context must be untouched.
	if dev, ok := cfg.Get("dev"); !ok || dev.BearerToken != "" {
		t.Errorf("dev context should be unchanged, got %+v (ok=%v)", dev, ok)
	}
}

func TestLoginServerReusesExistingHostnameContext(t *testing.T) {
	path := isolateHome(t)

	// A context already exists for the host (its name IS the hostname).
	if _, err := execute(t, "config", "create-context",
		"--name", "newhost", "--server", "http://newhost:8080/api/v1/", "--namespace", "team1"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	// Switch away so it isn't current.
	if _, err := execute(t, "config", "create-context",
		"--name", "other", "--server", "http://other/api/v1/"); err != nil {
		t.Fatalf("create-context other: %v", err)
	}

	if _, err := execute(t, "login", "--server", "http://newhost:8080/api/v1/", "--token", "tok"); err != nil {
		t.Fatalf("login --server: %v", err)
	}

	cfg, _ := config.Load(path)
	// No duplicate context was created.
	count := 0
	for _, c := range cfg.Contexts {
		if c.Name == "newhost" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one 'newhost' context, got %d", count)
	}
	// The existing context got the token, kept its namespace, and is current.
	ctx, _ := cfg.Get("newhost")
	if ctx.BearerToken != "tok" {
		t.Errorf("token = %q, want tok", ctx.BearerToken)
	}
	if ctx.Namespace != "team1" {
		t.Errorf("namespace = %q, want team1 (preserved)", ctx.Namespace)
	}
	if cfg.CurrentContext != "newhost" {
		t.Errorf("current context = %q, want newhost", cfg.CurrentContext)
	}
}

func TestLoginNoServerUsesCurrentContext(t *testing.T) {
	path := isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "a", "--server", "http://a/api/v1/"); err != nil {
		t.Fatalf("create-context a: %v", err)
	}
	// b is current.
	if _, err := execute(t, "config", "create-context",
		"--name", "b", "--server", "http://b/api/v1/"); err != nil {
		t.Fatalf("create-context b: %v", err)
	}

	before, _ := config.Load(path)
	countBefore := len(before.Contexts)

	// No --server: token goes on the current context (b), no new context.
	if _, err := execute(t, "login", "--token", "tok"); err != nil {
		t.Fatalf("login: %v", err)
	}

	cfg, _ := config.Load(path)
	if len(cfg.Contexts) != countBefore {
		t.Errorf("login without --server should not add a context: had %d, now %d", countBefore, len(cfg.Contexts))
	}
	b, _ := cfg.Get("b")
	if b.BearerToken != "tok" {
		t.Errorf("current context b token = %q, want tok", b.BearerToken)
	}
	if a, _ := cfg.Get("a"); a.BearerToken != "" {
		t.Errorf("non-current context a should be untouched, got token %q", a.BearerToken)
	}
}

// TestLoginSuggestsAuthStatus pins the pointer to `auth status`: the token's
// roles and audiences decide what now works, and login is when the user has them.
func TestLoginSuggestsAuthStatus(t *testing.T) {
	isolateHome(t)
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	out, err := execute(t, "login", "--token", "sekret")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if !strings.Contains(out, "rossoctl auth status") {
		t.Errorf("login should suggest `rossoctl auth status`:\n%s", out)
	}
}
