package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeKeycloakRegistrar serves the admin endpoints `keycloak register` uses.
//
// It is separate from fakeKeycloakServer because the two commands need opposite
// defaults: unregister's fake starts with the objects present, while register's
// starts with an empty realm so the create path is the default. Bodies are kept so
// a test can assert what was sent, not merely that something was.
type fakeKeycloakRegistrar struct {
	requests []string
	bodies   map[string]string
	// existingClients maps clientId -> internal uuid for clients that already
	// exist. The platform client is seeded by default since a real realm has one.
	existingClients map[string]string
	existingScopes  map[string]string
	clientRep       map[string]any // returned by GET on an existing client
	secret          string
}

func newFakeRegistrar(t *testing.T) (*fakeKeycloakRegistrar, *httptest.Server) {
	t.Helper()
	f := &fakeKeycloakRegistrar{
		bodies:          map[string]string{},
		existingClients: map[string]string{"rossoctl": "uuid-rossoctl"},
		existingScopes:  map[string]string{},
		secret:          "dev-secret-1234",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, key)
		if r.Body != nil {
			if b, err := io.ReadAll(r.Body); err == nil && len(b) > 0 {
				f.bodies[key] = string(b)
			}
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"access_token":"admin-token","expires_in":60}`))

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/clients"):
			want := r.URL.Query().Get("clientId")
			if uuid, ok := f.existingClients[want]; ok {
				_, _ = w.Write([]byte(`[{"id":"` + uuid + `","clientId":"` + want + `"}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/clients"):
			var rep struct {
				ClientID string `json:"clientId"`
			}
			_ = json.Unmarshal([]byte(f.bodies[key]), &rep)
			f.existingClients[rep.ClientID] = "uuid-created"
			w.Header().Set("Location", "http://kc/admin/realms/rossoctl/clients/uuid-created")
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/client-secret"):
			_, _ = w.Write([]byte(`{"value":"` + f.secret + `"}`))

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/client-scopes"):
			var out []string
			for name, id := range f.existingScopes {
				out = append(out, `{"id":"`+id+`","name":"`+name+`"}`)
			}
			_, _ = w.Write([]byte("[" + strings.Join(out, ",") + "]"))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/client-scopes"):
			var rep struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal([]byte(f.bodies[key]), &rep)
			f.existingScopes[rep.Name] = "uuid-scope"
			w.Header().Set("Location", "http://kc/admin/realms/rossoctl/client-scopes/uuid-scope")
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol-mappers/models"):
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && f.clientRep != nil:
			b, _ := json.Marshal(f.clientRep)
			_, _ = w.Write(b)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// TestKeycloakRegisterRequiresWorkload verifies the one required flag is enforced
// before anything is contacted, matching unregister.
func TestKeycloakRegisterRequiresWorkload(t *testing.T) {
	f, srv := newFakeRegistrar(t)

	out, err := execute(t, "keycloak", "register", "--keycloakURL", srv.URL, "--namespace", "ns1")
	if err == nil {
		t.Fatalf("omitting --workload should fail; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--workload") {
		t.Errorf("error should name the missing flag: %v", err)
	}
	if len(f.requests) != 0 {
		t.Errorf("nothing should be contacted without --workload; got %v", f.requests)
	}
}

// TestKeycloakRegisterCreatesTheFourObjects verifies a default run creates the
// client, the scope, its mapper, and all three attachments — at the endpoints
// `keycloak unregister` removes.
func TestKeycloakRegisterCreatesTheFourObjects(t *testing.T) {
	f, srv := newFakeRegistrar(t)

	out, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--namespace", "ns1",
		"--sa", "agent-sa", "--workload", "agent-a")
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}

	for _, want := range []string{
		"POST /admin/realms/rossoctl/clients",
		"POST /admin/realms/rossoctl/client-scopes",
		"POST /admin/realms/rossoctl/client-scopes/uuid-scope/protocol-mappers/models",
		"PUT /admin/realms/rossoctl/default-default-client-scopes/uuid-scope",
		"PUT /admin/realms/rossoctl/clients/uuid-created/default-client-scopes/uuid-scope",
		"PUT /admin/realms/rossoctl/clients/uuid-rossoctl/default-client-scopes/uuid-scope",
	} {
		if !sawRequest(f.requests, want) {
			t.Errorf("missing %q; got %v", want, f.requests)
		}
	}

	if !strings.Contains(out, "agent-ns1-agent-a-aud") {
		t.Errorf("output should name the scope it created:\n%s", out)
	}
	if !strings.Contains(out, "spiffe://localtest.me/ns/ns1/sa/agent-sa") {
		t.Errorf("output should name the client it created:\n%s", out)
	}
}

// TestKeycloakRegisterUsesTheSpiffeIDAsTheAudience verifies the audience is the
// client's own SPIFFE ID, as the operator sets it. A different audience produces
// tokens the workload's own inbound validation rejects.
func TestKeycloakRegisterUsesTheSpiffeIDAsTheAudience(t *testing.T) {
	f, srv := newFakeRegistrar(t)

	if _, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--namespace", "ns1",
		"--sa", "agent-sa", "--workload", "agent-a"); err != nil {
		t.Fatalf("register: %v", err)
	}

	body := f.bodies["POST /admin/realms/rossoctl/client-scopes/uuid-scope/protocol-mappers/models"]
	if body == "" {
		t.Fatalf("no mapper created; got %v", f.requests)
	}
	var rep struct {
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("decode mapper: %v", err)
	}
	const want = "spiffe://localtest.me/ns/ns1/sa/agent-sa"
	if got := rep.Config["included.custom.audience"]; got != want {
		t.Errorf("audience = %q, want the client's own SPIFFE ID %q", got, want)
	}
}

// TestKeycloakRegisterDefaultsToFederatedJWT verifies the auth-type default and
// that no secret is printed, since a federated-jwt client has none.
func TestKeycloakRegisterDefaultsToFederatedJWT(t *testing.T) {
	f, srv := newFakeRegistrar(t)

	out, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--namespace", "ns1", "--workload", "agent-a")
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}

	var rep struct {
		ClientAuthenticatorType string            `json:"clientAuthenticatorType"`
		Attributes              map[string]string `json:"attributes"`
	}
	_ = json.Unmarshal([]byte(f.bodies["POST /admin/realms/rossoctl/clients"]), &rep)
	if rep.ClientAuthenticatorType != "federated-jwt" {
		t.Errorf("clientAuthenticatorType = %q, want federated-jwt", rep.ClientAuthenticatorType)
	}
	if rep.Attributes["jwt.credential.issuer"] != "spire-spiffe" {
		t.Errorf("jwt.credential.issuer = %q", rep.Attributes["jwt.credential.issuer"])
	}
	if !strings.Contains(out, "no client secret") {
		t.Errorf("output should say there is no secret in federated-jwt mode:\n%s", out)
	}
	if strings.Contains(out, f.secret) {
		t.Errorf("no secret should be printed for federated-jwt:\n%s", out)
	}
}

// TestKeycloakRegisterPrintsTheSecretForClientSecret verifies the secret is printed
// when one exists, which is the only way a user can obtain it from this command.
func TestKeycloakRegisterPrintsTheSecretForClientSecret(t *testing.T) {
	f, srv := newFakeRegistrar(t)

	out, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--namespace", "ns1", "--workload", "agent-a",
		"--clientAuthType", "client-secret")
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}
	if !strings.Contains(out, f.secret) {
		t.Errorf("output should print the client secret:\n%s", out)
	}
}

// TestKeycloakRegisterRejectsABadAuthType verifies a typo fails rather than
// creating a client that cannot authenticate.
func TestKeycloakRegisterRejectsABadAuthType(t *testing.T) {
	_, srv := newFakeRegistrar(t)

	out, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--namespace", "ns1", "--workload", "agent-a",
		"--clientAuthType", "client-jwt")
	if err == nil {
		t.Fatalf("an unsupported auth type should fail; output:\n%s", out)
	}
}

// TestKeycloakRegisterSkipsAnExistingClient verifies an existing client is reused
// and never modified — it may be shared by sibling workloads on the same
// ServiceAccount — and that the output says so.
func TestKeycloakRegisterSkipsAnExistingClient(t *testing.T) {
	const clientID = "spiffe://localtest.me/ns/ns1/sa/agent-sa"
	f, srv := newFakeRegistrar(t)
	f.existingClients[clientID] = "uuid-existing"
	f.clientRep = map[string]any{
		"clientAuthenticatorType": "federated-jwt",
		"serviceAccountsEnabled":  true,
		"attributes":              map[string]string{"jwt.credential.sub": clientID},
	}

	out, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--namespace", "ns1",
		"--sa", "agent-sa", "--workload", "agent-a")
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}

	if sawRequest(f.requests, "POST /admin/realms/rossoctl/clients") {
		t.Error("an existing client must not be recreated")
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("output should say the client was reused:\n%s", out)
	}
	// The scope is still attached to the existing client: Keycloak does not apply
	// realm default-defaults to clients that already exist.
	if !sawRequest(f.requests, "PUT /admin/realms/rossoctl/clients/uuid-existing/default-client-scopes/uuid-scope") {
		t.Errorf("the scope should be attached to the existing client; got %v", f.requests)
	}
}

// TestKeycloakRegisterWarnsAboutClientDrift verifies a pre-existing client with the
// wrong authenticator type produces a warning rather than silent success.
func TestKeycloakRegisterWarnsAboutClientDrift(t *testing.T) {
	const clientID = "spiffe://localtest.me/ns/ns1/sa/agent-sa"
	f, srv := newFakeRegistrar(t)
	f.existingClients[clientID] = "uuid-existing"
	f.clientRep = map[string]any{
		"clientAuthenticatorType": "client-secret", // default would create federated-jwt
		"serviceAccountsEnabled":  true,
		"attributes":              map[string]string{},
	}

	out, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--namespace", "ns1",
		"--sa", "agent-sa", "--workload", "agent-a")
	if err != nil {
		t.Fatalf("drift should be reported, not fatal: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("output should warn about the mismatch:\n%s", out)
	}
	if !strings.Contains(out, "clientAuthenticatorType") {
		t.Errorf("the warning should name the field:\n%s", out)
	}
}

// TestKeycloakRegisterReusesAnExistingScope verifies an existing scope is reused,
// its mappers untouched, and that the output warns the audience was not verified.
func TestKeycloakRegisterReusesAnExistingScope(t *testing.T) {
	f, srv := newFakeRegistrar(t)
	f.existingScopes["agent-ns1-agent-a-aud"] = "uuid-old-scope"

	out, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--namespace", "ns1", "--workload", "agent-a")
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}

	if sawRequest(f.requests, "/protocol-mappers/models") {
		t.Error("an existing scope's mappers must not be touched")
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("output should say the scope was reused:\n%s", out)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("output should say the existing mapper was left alone:\n%s", out)
	}
}

// TestKeycloakRegisterUsesTheContextNamespace verifies --namespace defaults to the
// current context's namespace, as the flag's documentation promises.
func TestKeycloakRegisterUsesTheContextNamespace(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://unused.example/api/v1/", "--namespace", "team7"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	_, srv := newFakeRegistrar(t)

	out, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--workload", "agent-a")
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}
	if !strings.Contains(out, "/ns/team7/") {
		t.Errorf("the context's namespace should be used:\n%s", out)
	}
	if !strings.Contains(out, "agent-team7-agent-a-aud") {
		t.Errorf("the scope name should use the context's namespace:\n%s", out)
	}
}

// TestKeycloakRegisterRejectsABadRealm verifies a realm that would retarget the URL
// path is refused before any request is sent.
func TestKeycloakRegisterRejectsABadRealm(t *testing.T) {
	f, srv := newFakeRegistrar(t)

	out, err := execute(t, "keycloak", "register",
		"--keycloakURL", srv.URL, "--realm", "../master",
		"--namespace", "ns1", "--workload", "agent-a")
	if err == nil {
		t.Fatalf("a traversing realm should be rejected; output:\n%s", out)
	}
	if len(f.requests) != 0 {
		t.Errorf("nothing should be contacted with a bad realm; got %v", f.requests)
	}
}

// TestKeycloakRegisterRoundTripsWithUnregister verifies the names register derives
// are the ones unregister looks for, through the command layer with the same flags.
//
// This is the property that makes the pair usable: a divergence would leave objects
// behind that the CLI cannot remove.
func TestKeycloakRegisterRoundTripsWithUnregister(t *testing.T) {
	f, srv := newFakeRegistrar(t)

	args := []string{"--keycloakURL", srv.URL, "--namespace", "ns1",
		"--sa", "agent-sa", "--workload", "agent-a"}

	if out, err := execute(t, append([]string{"keycloak", "register"}, args...)...); err != nil {
		t.Fatalf("register: %v\n%s", err, out)
	}

	// The names register used, as sent to Keycloak.
	var scopeRep struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal([]byte(f.bodies["POST /admin/realms/rossoctl/client-scopes"]), &scopeRep)
	var clientRep struct {
		ClientID string `json:"clientId"`
	}
	_ = json.Unmarshal([]byte(f.bodies["POST /admin/realms/rossoctl/clients"]), &clientRep)

	// Serve those same objects to unregister and confirm it finds both.
	srv2, requests := fakeKeycloakServer(t, scopeRep.Name, clientRep.ClientID)
	out, err := execute(t, append([]string{"keycloak", "unregister", "--force"},
		"--keycloakURL", srv2.URL, "--namespace", "ns1",
		"--sa", "agent-sa", "--workload", "agent-a")...)
	if err != nil {
		t.Fatalf("unregister: %v\n%s", err, out)
	}
	if strings.Contains(out, "not found") {
		t.Errorf("unregister should find what register created:\n%s", out)
	}
	if !sawRequest(*requests, "DELETE /admin/realms/rossoctl/client-scopes/scope-uuid") {
		t.Errorf("the scope register created should be deleted; got %v", *requests)
	}
	if !sawRequest(*requests, "DELETE /admin/realms/rossoctl/clients/uuid-"+clientRep.ClientID) {
		t.Errorf("the client register created should be deleted; got %v", *requests)
	}
}

// TestKeycloakRegisterFlagDefaults pins the register-specific defaults, which are
// part of the requested interface.
func TestKeycloakRegisterFlagDefaults(t *testing.T) {
	resetFlags(rootCmd)

	reg, _, err := rootCmd.Find([]string{"keycloak", "register"})
	if err != nil {
		t.Fatalf("find keycloak register: %v", err)
	}
	for _, tc := range []struct{ flag, want string }{
		{"trustDomain", "localtest.me"},
		{"namespace", ""},
		{"sa", "default"},
		{"workload", ""},
		{"clientAuthType", "federated-jwt"},
		{"spiffeIDPAlias", "spire-spiffe"},
		{"tokenExchange", "false"},
	} {
		f := reg.Flags().Lookup(tc.flag)
		if f == nil {
			t.Errorf("keycloak register has no --%s flag", tc.flag)
			continue
		}
		if f.DefValue != tc.want {
			t.Errorf("--%s default = %q, want %q", tc.flag, f.DefValue, tc.want)
		}
	}
}
