package keycloakadmin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// registrar is a fake Keycloak that records creations, so a test can assert the
// representation sent rather than only that a request happened.
//
// It starts empty: the first Register against it takes the create path. Seeding
// clients or scopes exercises the reuse path.
type registrar struct {
	requests []string          // "METHOD /path" in order
	bodies   map[string]string // last JSON body per "METHOD /path"
	clients  map[string]string // clientId -> internal uuid
	scopes   map[string]string // scope name -> id
	client   map[string]any    // representation returned by GET on a seeded client
	status   map[string]int    // "METHOD /path" -> forced status
	secret   string
}

func newRegistrar() *registrar {
	return &registrar{
		bodies:  map[string]string{},
		clients: map[string]string{},
		scopes:  map[string]string{},
		status:  map[string]int{},
		secret:  "s3cr3t-value",
	}
}

func (f *registrar) server(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, key)
		if r.Body != nil {
			if b, err := io.ReadAll(r.Body); err == nil && len(b) > 0 {
				f.bodies[key] = string(b)
			}
		}
		if code, ok := f.status[key]; ok {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/clients"):
			want := r.URL.Query().Get("clientId")
			if uuid, ok := f.clients[want]; ok {
				_, _ = w.Write([]byte(`[{"id":"` + uuid + `","clientId":"` + want + `"}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/clients"):
			var rep clientRep
			_ = json.Unmarshal([]byte(f.bodies[key]), &rep)
			f.clients[rep.ClientID] = "uuid-new-client"
			w.Header().Set("Location", "http://kc/admin/realms/r/clients/uuid-new-client")
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/client-secret"):
			_, _ = w.Write([]byte(`{"value":"` + f.secret + `"}`))

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/client-scopes"):
			var out []string
			for name, id := range f.scopes {
				out = append(out, `{"id":"`+id+`","name":"`+name+`"}`)
			}
			_, _ = w.Write([]byte("[" + strings.Join(out, ",") + "]"))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/client-scopes"):
			var rep struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal([]byte(f.bodies[key]), &rep)
			f.scopes[rep.Name] = "uuid-new-scope"
			w.Header().Set("Location", "http://kc/admin/realms/r/client-scopes/uuid-new-scope")
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol-mappers/models"):
			w.WriteHeader(http.StatusCreated)

		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodGet && f.client != nil:
			// GET on a specific client, used by the drift check.
			b, _ := json.Marshal(f.client)
			_, _ = w.Write(b)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL}
}

func (f *registrar) indexOf(t *testing.T, key string) int {
	t.Helper()
	for i, r := range f.requests {
		if r == key {
			return i
		}
	}
	t.Fatalf("request %q was never made; got %v", key, f.requests)
	return -1
}

// baseRegisterOptions is a realistic registration, matching what the command layer
// derives for namespace ns1, workload agent-a, ServiceAccount agent-sa.
func baseRegisterOptions() RegisterOptions {
	const clientID = "spiffe://localtest.me/ns/ns1/sa/agent-sa"
	return RegisterOptions{
		Realm:      "rossoctl",
		ClientID:   clientID,
		ClientName: "ns1/agent-a",
		ScopeName:  "agent-ns1-agent-a-aud",
		Audience:   clientID,
		AuthType:   AuthTypeFederatedJWT,
		Platforms:  DefaultPlatformClientIDs,
	}
}

// TestRegisterCreatesTheClientRepresentationTheOperatorWould pins the client
// representation field by field.
//
// These fields are why the client works: without serviceAccountsEnabled it cannot
// obtain a token at all, and with fullScopeAllowed it would receive every realm
// scope, making the audience scope attachment meaningless.
func TestRegisterCreatesTheClientRepresentationTheOperatorWould(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	if _, err := c.Register(context.Background(), "tok", baseRegisterOptions()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	body := f.bodies["POST /admin/realms/rossoctl/clients"]
	if body == "" {
		t.Fatalf("no client was created; requests: %v", f.requests)
	}
	var rep clientRep
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("decode client rep: %v", err)
	}

	if rep.ClientID != "spiffe://localtest.me/ns/ns1/sa/agent-sa" {
		t.Errorf("clientId = %q", rep.ClientID)
	}
	if rep.Name != "ns1/agent-a" {
		t.Errorf("name = %q, want the operator's <namespace>/<workload>", rep.Name)
	}
	if !rep.ServiceAccountsEnabled {
		t.Error("serviceAccountsEnabled must be true or the client cannot get a token")
	}
	if rep.FullScopeAllowed {
		t.Error("fullScopeAllowed must be false or the audience scope attachment is moot")
	}
	if rep.PublicClient {
		t.Error("publicClient must be false")
	}
	if !rep.StandardFlowEnabled || !rep.DirectAccessGrantsEnabled {
		t.Error("standardFlow and directAccessGrants should match the operator (both true)")
	}
}

// TestRegisterFederatedJWTSetsTheSpiffeSubject verifies the attributes that tie
// the Keycloak client to the SPIFFE identity presenting the JWT. A wrong subject
// means Keycloak rejects the workload's own credential.
func TestRegisterFederatedJWTSetsTheSpiffeSubject(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	o := baseRegisterOptions()
	o.AuthType = AuthTypeFederatedJWT
	if _, err := c.Register(context.Background(), "tok", o); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var rep clientRep
	_ = json.Unmarshal([]byte(f.bodies["POST /admin/realms/rossoctl/clients"]), &rep)

	if rep.ClientAuthenticatorType != AuthTypeFederatedJWT {
		t.Errorf("clientAuthenticatorType = %q, want %q", rep.ClientAuthenticatorType, AuthTypeFederatedJWT)
	}
	if got := rep.Attributes["jwt.credential.sub"]; got != o.ClientID {
		t.Errorf("jwt.credential.sub = %q, want the SPIFFE ID %q", got, o.ClientID)
	}
	if got := rep.Attributes["jwt.credential.issuer"]; got != DefaultSpiffeIDPAlias {
		t.Errorf("jwt.credential.issuer = %q, want %q", got, DefaultSpiffeIDPAlias)
	}
}

// TestRegisterFederatedJWTReadsNoSecret verifies no secret is fetched or reported
// in federated-jwt mode. Keycloak returns an unused placeholder for such clients,
// and surfacing it would look like a usable credential.
func TestRegisterFederatedJWTReadsNoSecret(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	o := baseRegisterOptions()
	o.AuthType = AuthTypeFederatedJWT
	res, err := c.Register(context.Background(), "tok", o)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if res.ClientSecret != "" {
		t.Errorf("federated-jwt should report no secret, got %q", res.ClientSecret)
	}
	for _, r := range f.requests {
		if strings.HasSuffix(r, "/client-secret") {
			t.Errorf("the secret endpoint must not be called for federated-jwt; got %v", f.requests)
		}
	}
}

// TestRegisterClientSecretReadsTheSecret verifies the secret is fetched and
// returned in client-secret mode, and that no SPIFFE JWT attributes are set.
func TestRegisterClientSecretReadsTheSecret(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	o := baseRegisterOptions()
	o.AuthType = AuthTypeClientSecret
	res, err := c.Register(context.Background(), "tok", o)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if res.ClientSecret != f.secret {
		t.Errorf("ClientSecret = %q, want %q", res.ClientSecret, f.secret)
	}
	var rep clientRep
	_ = json.Unmarshal([]byte(f.bodies["POST /admin/realms/rossoctl/clients"]), &rep)
	if _, ok := rep.Attributes["jwt.credential.sub"]; ok {
		t.Error("client-secret clients should carry no jwt.credential.sub")
	}
}

// TestRegisterDefaultsAuthTypeToClientSecret verifies an unset AuthType matches
// the operator's own fallback, so a caller of this package gets the operator's
// behaviour rather than the CLI's flag default.
func TestRegisterDefaultsAuthTypeToClientSecret(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	o := baseRegisterOptions()
	o.AuthType = ""
	res, err := c.Register(context.Background(), "tok", o)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if res.AuthType != AuthTypeClientSecret {
		t.Errorf("AuthType = %q, want %q", res.AuthType, AuthTypeClientSecret)
	}
}

// TestRegisterRejectsAnUnknownAuthType verifies a typo fails rather than being
// sent to Keycloak, which would accept it and create a client that cannot
// authenticate.
func TestRegisterRejectsAnUnknownAuthType(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	o := baseRegisterOptions()
	o.AuthType = "client-jwt"
	if _, err := c.Register(context.Background(), "tok", o); err == nil {
		t.Fatal("an unsupported auth type should be rejected")
	}
	if len(f.requests) != 0 {
		t.Errorf("nothing should be sent for a bad auth type; got %v", f.requests)
	}
}

// TestRegisterCreatesTheAudienceMapper pins the mapper config, which is the object
// that actually puts the audience in the token. The claim flags matter: the
// audience belongs in the access token, which is what one workload presents to
// another.
func TestRegisterCreatesTheAudienceMapper(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	o := baseRegisterOptions()
	if _, err := c.Register(context.Background(), "tok", o); err != nil {
		t.Fatalf("Register: %v", err)
	}

	body := f.bodies["POST /admin/realms/rossoctl/client-scopes/uuid-new-scope/protocol-mappers/models"]
	if body == "" {
		t.Fatalf("no mapper was created; requests: %v", f.requests)
	}
	var rep struct {
		Name           string            `json:"name"`
		Protocol       string            `json:"protocol"`
		ProtocolMapper string            `json:"protocolMapper"`
		Config         map[string]string `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("decode mapper: %v", err)
	}

	if rep.ProtocolMapper != oidcAudienceMapper {
		t.Errorf("protocolMapper = %q, want %q", rep.ProtocolMapper, oidcAudienceMapper)
	}
	if rep.Protocol != "openid-connect" {
		t.Errorf("protocol = %q", rep.Protocol)
	}
	if rep.Name != o.ScopeName {
		t.Errorf("mapper name = %q, want the scope name %q", rep.Name, o.ScopeName)
	}
	if got := rep.Config["included.custom.audience"]; got != o.Audience {
		t.Errorf("included.custom.audience = %q, want %q", got, o.Audience)
	}
	if rep.Config["access.token.claim"] != "true" {
		t.Error("access.token.claim must be true or the audience never reaches the token")
	}
	if rep.Config["id.token.claim"] != "false" || rep.Config["userinfo.token.claim"] != "false" {
		t.Error("id.token.claim and userinfo.token.claim should be false, as the operator sets them")
	}
}

// TestRegisterCreatesTheScopeWithTokenScopeIncluded verifies the scope attribute
// that puts it in the token's scope claim.
func TestRegisterCreatesTheScopeWithTokenScopeIncluded(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	if _, err := c.Register(context.Background(), "tok", baseRegisterOptions()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var rep struct {
		Name       string            `json:"name"`
		Protocol   string            `json:"protocol"`
		Attributes map[string]string `json:"attributes"`
	}
	_ = json.Unmarshal([]byte(f.bodies["POST /admin/realms/rossoctl/client-scopes"]), &rep)

	if rep.Name != "agent-ns1-agent-a-aud" {
		t.Errorf("scope name = %q", rep.Name)
	}
	if rep.Protocol != "openid-connect" {
		t.Errorf("scope protocol = %q", rep.Protocol)
	}
	if rep.Attributes["include.in.token.scope"] != "true" {
		t.Error("include.in.token.scope must be true, as the operator sets it")
	}
}

// TestRegisterLinksTheScopeToRealmClientAndPlatform verifies all three attachments
// are made, at the exact endpoints `keycloak unregister` removes.
//
// The client attachment is the one easily missed: Keycloak does not apply a realm
// default-default scope to clients that already exist when the scope appears, so
// without it a reused client never carries the audience.
func TestRegisterLinksTheScopeToRealmClientAndPlatform(t *testing.T) {
	f := newRegistrar()
	f.clients["rossoctl"] = "uuid-rossoctl"
	c := f.server(t)

	if _, err := c.Register(context.Background(), "tok", baseRegisterOptions()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	for _, want := range []string{
		"PUT /admin/realms/rossoctl/default-default-client-scopes/uuid-new-scope",
		"PUT /admin/realms/rossoctl/clients/uuid-new-client/default-client-scopes/uuid-new-scope",
		"PUT /admin/realms/rossoctl/clients/uuid-rossoctl/default-client-scopes/uuid-new-scope",
	} {
		f.indexOf(t, want)
	}
}

// TestRegisterCreatesTheClientBeforeTheScope verifies the ordering the operator
// uses. The scope is attached to the client, so the client has to exist first.
func TestRegisterCreatesTheClientBeforeTheScope(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	if _, err := c.Register(context.Background(), "tok", baseRegisterOptions()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	client := f.indexOf(t, "POST /admin/realms/rossoctl/clients")
	scope := f.indexOf(t, "POST /admin/realms/rossoctl/client-scopes")
	mapper := f.indexOf(t, "POST /admin/realms/rossoctl/client-scopes/uuid-new-scope/protocol-mappers/models")
	attach := f.indexOf(t, "PUT /admin/realms/rossoctl/clients/uuid-new-client/default-client-scopes/uuid-new-scope")

	if !(client < scope && scope < mapper && mapper < attach) {
		t.Errorf("want client < scope < mapper < attach; got %d, %d, %d, %d in %v",
			client, scope, mapper, attach, f.requests)
	}
}

// TestRegisterReusesAnExistingClient verifies an existing client is neither
// recreated nor modified. It is keyed on the ServiceAccount, so it may belong to
// sibling workloads.
func TestRegisterReusesAnExistingClient(t *testing.T) {
	o := baseRegisterOptions()
	f := newRegistrar()
	f.clients[o.ClientID] = "uuid-existing"
	f.client = map[string]any{
		"clientAuthenticatorType": AuthTypeFederatedJWT,
		"serviceAccountsEnabled":  true,
		"attributes":              map[string]string{"jwt.credential.sub": o.ClientID},
	}
	c := f.server(t)

	res, err := c.Register(context.Background(), "tok", o)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if res.ClientCreated {
		t.Error("an existing client should not be reported as created")
	}
	if !res.ClientExisted {
		t.Error("ClientExisted should be set")
	}
	if len(res.Drift) != 0 {
		t.Errorf("a matching client should report no drift; got %v", res.Drift)
	}
	for _, r := range f.requests {
		if r == "POST /admin/realms/rossoctl/clients" {
			t.Error("an existing client must not be re-POSTed")
		}
		if r == "PUT /admin/realms/rossoctl/clients/uuid-existing" {
			t.Error("an existing client must not be modified")
		}
	}
}

// TestRegisterReportsClientDrift verifies a pre-existing client whose auth type
// differs is reported. Silence here would look like success while the workload
// cannot authenticate.
func TestRegisterReportsClientDrift(t *testing.T) {
	o := baseRegisterOptions()
	f := newRegistrar()
	f.clients[o.ClientID] = "uuid-existing"
	f.client = map[string]any{
		"clientAuthenticatorType": AuthTypeClientSecret, // wanted federated-jwt
		"serviceAccountsEnabled":  true,
		"attributes":              map[string]string{},
	}
	c := f.server(t)

	res, err := c.Register(context.Background(), "tok", o)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if len(res.Drift) == 0 {
		t.Fatal("a client with the wrong authenticator type should report drift")
	}
	joined := strings.Join(res.Drift, "; ")
	if !strings.Contains(joined, AuthTypeClientSecret) || !strings.Contains(joined, AuthTypeFederatedJWT) {
		t.Errorf("drift should name both the actual and wanted type: %q", joined)
	}
	// Reported, not corrected.
	for _, r := range f.requests {
		if strings.HasPrefix(r, "PUT /admin/realms/rossoctl/clients/uuid-existing") &&
			!strings.Contains(r, "default-client-scopes") {
			t.Errorf("drift must not be corrected; saw %q", r)
		}
	}
}

// TestRegisterReportsAMissingServiceAccount verifies drift covers the flag without
// which the client cannot obtain a token at all.
func TestRegisterReportsAMissingServiceAccount(t *testing.T) {
	o := baseRegisterOptions()
	f := newRegistrar()
	f.clients[o.ClientID] = "uuid-existing"
	f.client = map[string]any{
		"clientAuthenticatorType": AuthTypeFederatedJWT,
		"serviceAccountsEnabled":  false,
		"attributes":              map[string]string{"jwt.credential.sub": o.ClientID},
	}
	c := f.server(t)

	res, err := c.Register(context.Background(), "tok", o)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !strings.Contains(strings.Join(res.Drift, "; "), "serviceAccountsEnabled") {
		t.Errorf("drift should name serviceAccountsEnabled; got %v", res.Drift)
	}
}

// TestRegisterReusesAnExistingScopeWithoutTouchingItsMappers verifies the
// operator's rule that an existing scope's mappers are left alone, so a
// concurrent reconcile is never fought over.
func TestRegisterReusesAnExistingScopeWithoutTouchingItsMappers(t *testing.T) {
	f := newRegistrar()
	f.scopes["agent-ns1-agent-a-aud"] = "uuid-old-scope"
	c := f.server(t)

	res, err := c.Register(context.Background(), "tok", baseRegisterOptions())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if res.ScopeCreated {
		t.Error("an existing scope should not be reported as created")
	}
	if !res.ScopeExisted {
		t.Error("ScopeExisted should be set")
	}
	if res.MapperCreated {
		t.Error("no mapper should be created for an existing scope")
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/protocol-mappers/models") {
			t.Errorf("an existing scope's mappers must not be touched; saw %q", r)
		}
	}
	// The existing scope's id is what gets linked, not a newly created one.
	f.indexOf(t, "PUT /admin/realms/rossoctl/default-default-client-scopes/uuid-old-scope")
}

// TestRegisterIsIdempotent verifies a second run against a fully registered realm
// creates nothing and still reports success, so a user can re-run to finish a
// partial registration.
func TestRegisterIsIdempotent(t *testing.T) {
	o := baseRegisterOptions()
	f := newRegistrar()
	f.clients["rossoctl"] = "uuid-rossoctl"
	f.client = map[string]any{
		"clientAuthenticatorType": AuthTypeFederatedJWT,
		"serviceAccountsEnabled":  true,
		"attributes":              map[string]string{"jwt.credential.sub": o.ClientID},
	}
	c := f.server(t)

	if _, err := c.Register(context.Background(), "tok", o); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	f.requests = nil

	res, err := c.Register(context.Background(), "tok", o)
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}
	if res.ClientCreated || res.ScopeCreated || res.MapperCreated {
		t.Errorf("second run should create nothing; got %+v", res)
	}
	if !res.RealmLinked || !res.ClientLinked {
		t.Error("the attachments should still be asserted on a re-run")
	}
	for _, r := range f.requests {
		if r == "POST /admin/realms/rossoctl/clients" || r == "POST /admin/realms/rossoctl/client-scopes" {
			t.Errorf("second run must not create objects; saw %q", r)
		}
	}
}

// TestRegisterTolerates409OnAttachment verifies an already-present link is treated
// as success. Keycloak answers 409 there, which is the state wanted.
func TestRegisterTolerates409OnAttachment(t *testing.T) {
	f := newRegistrar()
	f.status["PUT /admin/realms/rossoctl/default-default-client-scopes/uuid-new-scope"] = http.StatusConflict
	c := f.server(t)

	res, err := c.Register(context.Background(), "tok", baseRegisterOptions())
	if err != nil {
		t.Fatalf("a 409 on attachment should not fail: %v", err)
	}
	if !res.RealmLinked {
		t.Error("a 409 means already linked, so RealmLinked should be set")
	}
}

// TestRegisterReportsAFailedAttachment verifies a genuine failure is surfaced
// rather than swallowed. The operator ignores these errors because it will
// reconcile again; a one-shot CLI has no second chance, so it reports.
func TestRegisterReportsAFailedAttachment(t *testing.T) {
	f := newRegistrar()
	f.status["PUT /admin/realms/rossoctl/default-default-client-scopes/uuid-new-scope"] = http.StatusInternalServerError
	c := f.server(t)

	if _, err := c.Register(context.Background(), "tok", baseRegisterOptions()); err == nil {
		t.Fatal("a 500 on attachment should fail the call")
	}
}

// TestRegisterFailsOn404Attachment verifies a 404 is an error on the create path,
// unlike on the delete path where it means already-gone. Here it means the id just
// obtained does not exist, which is a real failure.
func TestRegisterFailsOn404Attachment(t *testing.T) {
	f := newRegistrar()
	f.status["PUT /admin/realms/rossoctl/default-default-client-scopes/uuid-new-scope"] = http.StatusNotFound
	c := f.server(t)

	if _, err := c.Register(context.Background(), "tok", baseRegisterOptions()); err == nil {
		t.Fatal("a 404 when linking a just-created scope should fail")
	}
}

// TestRegisterSkipsAnAbsentPlatformClient verifies a platform client that is not
// deployed is skipped, as the operator's own loop does.
func TestRegisterSkipsAnAbsentPlatformClient(t *testing.T) {
	f := newRegistrar() // no "rossoctl" client seeded
	c := f.server(t)

	res, err := c.Register(context.Background(), "tok", baseRegisterOptions())
	if err != nil {
		t.Fatalf("an absent platform client should not fail: %v", err)
	}
	if len(res.PlatformLinked) != 0 {
		t.Errorf("no platform should be reported linked; got %v", res.PlatformLinked)
	}
}

// TestRegisterRejectsATraversingRealm verifies the realm check applies to the
// create path too, and before anything is sent.
func TestRegisterRejectsATraversingRealm(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	o := baseRegisterOptions()
	o.Realm = "../master"
	if _, err := c.Register(context.Background(), "tok", o); err == nil {
		t.Fatal("a traversing realm should be rejected")
	}
	if len(f.requests) != 0 {
		t.Errorf("nothing should be sent for a bad realm; got %v", f.requests)
	}
}

// TestRegisterResolvesTheIDFromLocation verifies the created object's id comes
// from the Location header, which is the only place Keycloak reports it on a 201.
func TestRegisterResolvesTheIDFromLocation(t *testing.T) {
	f := newRegistrar()
	c := f.server(t)

	res, err := c.Register(context.Background(), "tok", baseRegisterOptions())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if res.ClientUUID != "uuid-new-client" {
		t.Errorf("ClientUUID = %q, want the id from Location", res.ClientUUID)
	}
}

// TestRegisterAndUnregisterAgreeOnNames is the round-trip that matters: what
// register creates is what unregister looks for. A divergence here would leave
// objects behind with no way to remove them via the CLI.
func TestRegisterAndUnregisterAgreeOnNames(t *testing.T) {
	const (
		trustDomain = "localtest.me"
		namespace   = "ns1"
		sa          = "agent-sa"
		workload    = "agent-a"
	)
	clientID := SpiffeClientID(trustDomain, namespace, sa)
	scopeName := AudienceScopeName(namespace, workload)

	f := newRegistrar()
	f.clients["rossoctl"] = "uuid-rossoctl"
	c := f.server(t)

	o := baseRegisterOptions()
	o.ClientID, o.Audience, o.ScopeName = clientID, clientID, scopeName
	if _, err := c.Register(context.Background(), "tok", o); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The fake now holds exactly what registration created; unregister it.
	res, err := c.Unregister(context.Background(), "tok", UnregisterOptions{
		Realm:        o.Realm,
		ClientID:     clientID,
		ScopeName:    scopeName,
		Platforms:    DefaultPlatformClientIDs,
		DeleteClient: true,
	})
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if !res.ScopeDeleted {
		t.Error("unregister should find and delete the scope register created")
	}
	if !res.ClientDeleted {
		t.Error("unregister should find and delete the client register created")
	}
	if res.ScopeAbsent || res.ClientAbsent {
		t.Errorf("nothing should be absent right after registering: %+v", res)
	}
}
