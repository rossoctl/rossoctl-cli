package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestLoginContextStoresTokenOnThatContext covers --context: the token must land
// on the named context, not the current one. Storing it on the current context
// would put a token issued by one server against another server's URL, since
// --context already decides which server the login talks to.
func TestLoginContextStoresTokenOnThatContext(t *testing.T) {
	path := isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "target", "--server", "http://target/api/v1/"); err != nil {
		t.Fatalf("create-context target: %v", err)
	}
	// current is created second, so it is the current context.
	if _, err := execute(t, "config", "create-context",
		"--name", "current", "--server", "http://current/api/v1/"); err != nil {
		t.Fatalf("create-context current: %v", err)
	}

	out, err := execute(t, "--context", "target", "login", "--token", "tok")
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}

	cfg, _ := config.Load(path)
	target, _ := cfg.Get("target")
	if target.BearerToken != "tok" {
		t.Errorf("target token = %q, want tok", target.BearerToken)
	}
	if cur, _ := cfg.Get("current"); cur.BearerToken != "" {
		t.Errorf("the current context was written to; token = %q, want empty", cur.BearerToken)
	}
	// The report has to name where the token actually went, or a user cannot
	// tell this case from the bug it replaces.
	if !strings.Contains(out, `"target"`) {
		t.Errorf("output does not name the target context:\n%s", out)
	}
}

// TestLoginContextLeavesCurrentUnchanged pins that --context does not switch the
// persistent selection. Unlike --server it names a context that already exists,
// so changing which one is current as a side effect would be a surprise.
func TestLoginContextLeavesCurrentUnchanged(t *testing.T) {
	path := isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "target", "--server", "http://target/api/v1/"); err != nil {
		t.Fatalf("create-context target: %v", err)
	}
	if _, err := execute(t, "config", "create-context",
		"--name", "current", "--server", "http://current/api/v1/"); err != nil {
		t.Fatalf("create-context current: %v", err)
	}

	if _, err := execute(t, "--context", "target", "login", "--token", "tok"); err != nil {
		t.Fatalf("login: %v", err)
	}

	cfg, _ := config.Load(path)
	if cfg.CurrentContext != "current" {
		t.Errorf("current context = %q, want it left at %q", cfg.CurrentContext, "current")
	}
}

// TestLoginContextRejectsUnknownContext verifies --context never creates a
// context, matching resolveContext: a typo must fail rather than silently
// bringing a half-configured context into existence and reporting success.
func TestLoginContextRejectsUnknownContext(t *testing.T) {
	path := isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "real", "--server", "http://real/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	out, err := execute(t, "--context", "nosuch", "login", "--token", "tok")
	if err == nil {
		t.Fatalf("login with an unknown --context succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("error = %v, want it to name the unknown context", err)
	}

	cfg, _ := config.Load(path)
	if _, ok := cfg.Get("nosuch"); ok {
		t.Error("the unknown context was created")
	}
	if real, _ := cfg.Get("real"); real.BearerToken != "" {
		t.Errorf("a token was stored despite the failure: %q", real.BearerToken)
	}
}

// TestLoginServerBeatsContext pins the precedence between the two flags, which
// was settled before --context was honored here: --server may create a context
// and make it current, so it stays the stronger selector.
func TestLoginServerBeatsContext(t *testing.T) {
	path := isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "target", "--server", "http://target/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if _, err := execute(t, "--context", "target",
		"--server", "http://other.example:8080/api/v1/", "login", "--token", "tok"); err != nil {
		t.Fatalf("login: %v", err)
	}

	cfg, _ := config.Load(path)
	if target, _ := cfg.Get("target"); target.BearerToken != "" {
		t.Errorf("--context won over --server; target token = %q", target.BearerToken)
	}
	byHost, ok := cfg.Get(config.ContextNameForServer("http://other.example:8080/api/v1/"))
	if !ok {
		t.Fatal("--server did not create its hostname context")
	}
	if byHost.BearerToken != "tok" {
		t.Errorf("hostname context token = %q, want tok", byHost.BearerToken)
	}
}

// TestLoginSuggestsAuthStatus pins the pointer to `auth status`: the token's
// roles and audiences decide what now works, and login is when the user has them.
// --- login --cortex ---

// TestLoginCortexSelectsCortexContext covers the basic outcome: the cortex
// context becomes current, carrying the seeded type and server, with no token.
func TestLoginCortexSelectsCortexContext(t *testing.T) {
	path := isolateHome(t)

	out, err := execute(t, "login", "--cortex")
	if err != nil {
		t.Fatalf("login --cortex: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cur, ok := cfg.Current()
	if !ok {
		t.Fatal("no current context after login --cortex")
	}
	if cur.Name != config.CortexContextName {
		t.Errorf("current context = %q, want %q", cur.Name, config.CortexContextName)
	}
	if cur.Type != config.TypeCortex {
		t.Errorf("cortex context type = %q, want %q", cur.Type, config.TypeCortex)
	}
	if cur.Server != config.DefaultCortexServer {
		t.Errorf("cortex context server = %q, want %q", cur.Server, config.DefaultCortexServer)
	}
	// A cortex context is answered in-process and credentials are ignored, so
	// login must not invent a token for it.
	if cur.BearerToken != "" {
		t.Errorf("token = %q, want empty: the local cortex needs none", cur.BearerToken)
	}
}

// TestLoginCortexMakesNoNetworkCall is the test that pins "without trying to
// contact a remote api server".
//
// The current context points at a server whose handler fails the test on any
// request at all, so a login that reached for /auth/config, ran a device flow, or
// listed namespaces remotely would be caught whichever of the three it tried.
// Every other test in this group would pass against an implementation that
// quietly dialed.
func TestLoginCortexMakesNoNetworkCall(t *testing.T) {
	isolateHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("login --cortex contacted the server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	// Make that server the current context, so a login that consults the current
	// context's server hits the failing handler.
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if out, err := execute(t, "login", "--cortex"); err != nil {
		t.Fatalf("login --cortex: %v\n%s", err, out)
	}
}

// TestLoginCortexCreatesContextWhenAbsent covers the config that predates cortex
// seeding: contexts exist, but none is named cortex.
func TestLoginCortexCreatesContextWhenAbsent(t *testing.T) {
	path := isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/", "--namespace", "team1"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if out, err := execute(t, "login", "--cortex"); err != nil {
		t.Fatalf("login --cortex: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cfg.Get(config.CortexContextName); !ok {
		t.Fatalf("login --cortex did not create the context: %+v", cfg.Contexts)
	}
	if cfg.CurrentContext != config.CortexContextName {
		t.Errorf("current context = %q, want %q", cfg.CurrentContext, config.CortexContextName)
	}
	// The pre-existing context must survive untouched.
	if dev, ok := cfg.Get("dev"); !ok || dev.Namespace != "team1" {
		t.Errorf("pre-existing context was disturbed: %+v", cfg.Contexts)
	}
}

// TestLoginCortexReusesExistingContext verifies an existing cortex context is
// selected rather than replaced. Catches an unconditional Upsert, which would
// silently discard a namespace or token the user had set.
func TestLoginCortexReusesExistingContext(t *testing.T) {
	path := isolateHome(t)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg.Upsert(config.Context{
		Name:        config.CortexContextName,
		Type:        config.TypeCortex,
		Server:      "http://localhost:9999/custom/",
		Namespace:   "mine",
		BearerToken: "keepme",
	})
	cfg.Upsert(config.Context{Name: "dev", Server: "http://dev/api/v1/"})
	if err := cfg.SetCurrent("dev"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	if out, err := execute(t, "login", "--cortex"); err != nil {
		t.Fatalf("login --cortex: %v\n%s", err, out)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	cur, ok := reloaded.Current()
	if !ok || cur.Name != config.CortexContextName {
		t.Fatalf("current context = %+v, want cortex", cur)
	}
	if cur.Server != "http://localhost:9999/custom/" {
		t.Errorf("server = %q, want the user's own value preserved", cur.Server)
	}
	if cur.Namespace != "mine" {
		t.Errorf("namespace = %q, want mine preserved", cur.Namespace)
	}
	if cur.BearerToken != "keepme" {
		t.Errorf("token = %q, want keepme preserved", cur.BearerToken)
	}
}

// TestLoginCortexPicksUpLocalNamespace verifies the namespace is taken from the
// local instance records through the in-process transport — no network involved.
//
// instances resolves its directory from the same $HOME that isolateHome sets, so
// creating the namespace directory is the whole fixture.
func TestLoginCortexPicksUpLocalNamespace(t *testing.T) {
	path := isolateHome(t)

	// path is <home>/.config/rossoctl/config.yaml, and the instance records live
	// under <home>/.config/rossoctl/namespaces, so this is a sibling of the config
	// file.
	nsDir := filepath.Join(filepath.Dir(path), "namespaces", "team7")
	if err := os.MkdirAll(nsDir, 0o700); err != nil {
		t.Fatalf("creating namespace dir: %v", err)
	}

	out, err := execute(t, "login", "--cortex")
	if err != nil {
		t.Fatalf("login --cortex: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cur, _ := cfg.Current()
	if cur.Namespace != "team7" {
		t.Errorf("namespace = %q, want team7 from the local instance records", cur.Namespace)
	}
	if !strings.Contains(out, "team7") {
		t.Errorf("output should name the namespace:\n%s", out)
	}
}

// TestLoginCortexBlankNamespaceIsReported is the complement: with no local
// records the namespace stays blank, and the output says so rather than implying
// the context is ready to use.
func TestLoginCortexBlankNamespaceIsReported(t *testing.T) {
	path := isolateHome(t)

	out, err := execute(t, "login", "--cortex")
	if err != nil {
		t.Fatalf("login --cortex: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cur, _ := cfg.Current()
	if cur.Namespace != "" {
		t.Errorf("namespace = %q, want empty with no local records", cur.Namespace)
	}
	if !strings.Contains(out, "No local namespaces found") {
		t.Errorf("output should report the blank namespace:\n%s", out)
	}
}

// TestLoginCortexRejectsConflictingFlags verifies each contradictory combination
// errors and names the offending flag, rather than silently picking a precedence.
func TestLoginCortexRejectsConflictingFlags(t *testing.T) {
	for _, c := range []struct{ flag, value string }{
		{"token", "sekret"},
		{"server", "http://elsewhere/api/v1/"},
		{"context", "dev"},
	} {
		t.Run(c.flag, func(t *testing.T) {
			isolateHome(t)
			out, err := execute(t, "login", "--cortex", "--"+c.flag, c.value)
			if err == nil {
				t.Fatalf("login --cortex --%s should have failed:\n%s", c.flag, out)
			}
			if !strings.Contains(err.Error(), "--"+c.flag) {
				t.Errorf("error should name --%s: %v", c.flag, err)
			}
		})
	}
}

// TestLoginSeedsCortexContextToo is the regression guard for the seeding half of
// this change: a plain login on an empty config gains a cortex context, while the
// API context stays current and receives the token.
func TestLoginSeedsCortexContextToo(t *testing.T) {
	path := isolateHome(t)

	if _, err := execute(t, "login", "--token", "tok"); err != nil {
		t.Fatalf("login: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cfg.Get(config.CortexContextName); !ok {
		t.Errorf("login did not seed a cortex context: %+v", cfg.Contexts)
	}
	// The regular work must be unchanged: the API context is current and holds
	// the token, and the cortex context holds none.
	cur, ok := cfg.Current()
	if !ok {
		t.Fatal("no current context")
	}
	if cur.Name != "rossoctl-ui.localtest.me" {
		t.Errorf("current context = %q, want the default-server context to stay current", cur.Name)
	}
	if cur.BearerToken != "tok" {
		t.Errorf("token = %q, want tok on the API context", cur.BearerToken)
	}
	if cortex, ok := cfg.Get(config.CortexContextName); ok && cortex.BearerToken != "" {
		t.Errorf("cortex token = %q, want empty", cortex.BearerToken)
	}
}

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
