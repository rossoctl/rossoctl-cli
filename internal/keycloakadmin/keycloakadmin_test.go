package keycloakadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// fakeKeycloak is a stand-in admin API that records every request it receives, so
// a test can assert on the exact endpoints called and their order.
//
// Recording the method and path rather than stubbing individual calls is what
// makes the ordering assertions possible, and ordering is a correctness property
// here: unlinking has to precede deletion.
type fakeKeycloak struct {
	srv *httptest.Server

	// requests is "METHOD /path" per received request, in order.
	requests []string

	// clients and scopes are the realm's contents, keyed by name/clientId.
	clients map[string]string // clientId -> internal UUID
	scopes  map[string]string // scope name -> scope id

	// status overrides the response for a "METHOD /path" prefix match.
	status map[string]int
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	f := &fakeKeycloak{
		clients: map[string]string{},
		scopes:  map[string]string{},
		status:  map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKeycloak) handle(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	f.requests = append(f.requests, key)

	for prefix, code := range f.status {
		if strings.HasPrefix(key, prefix) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"forced"}`))
			return
		}
	}

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"admin-token","expires_in":60}`))

	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/clients"):
		want := r.URL.Query().Get("clientId")
		out := []map[string]string{}
		if id, ok := f.clients[want]; ok {
			out = append(out, map[string]string{"id": id, "clientId": want})
		}
		writeJSON(w, out)

	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/client-scopes"):
		out := []map[string]string{}
		for name, id := range f.scopes {
			out = append(out, map[string]string{"id": id, "name": name})
		}
		writeJSON(w, out)

	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// client returns a Client pointed at the fake.
func (f *fakeKeycloak) client() *Client {
	return &Client{BaseURL: f.srv.URL}
}

// indexOf reports the position of the first request matching key, or -1.
func (f *fakeKeycloak) indexOf(key string) int {
	return slices.Index(f.requests, key)
}

const (
	testRealm    = "rossoctl"
	testClientID = "spiffe://localtest.me/ns/ns1/sa/agent-sa"
)

// TestSpiffeClientIDMatchesTheOperator pins the client ID format against the
// operator's resolveKeycloakClientID. A mismatch here means this command looks
// for a client that was never registered and silently reports it absent.
//
// Note the workload name is absent by design: the operator keys the client on the
// ServiceAccount.
func TestSpiffeClientIDMatchesTheOperator(t *testing.T) {
	got := SpiffeClientID("localtest.me", "ns1", "agent-sa")
	if want := "spiffe://localtest.me/ns/ns1/sa/agent-sa"; got != want {
		t.Errorf("SpiffeClientID = %q, want %q", got, want)
	}
	// The scope name distinguishes two workloads sharing a ServiceAccount, but the
	// client ID does not. That asymmetry is the reason deleting the client is
	// opt-in, so it is asserted rather than left as a comment.
	a := SpiffeClientID("localtest.me", "ns1", "shared-sa")
	b := SpiffeClientID("localtest.me", "ns1", "shared-sa")
	if a != b {
		t.Errorf("two workloads under one ServiceAccount should share a client ID: %q vs %q", a, b)
	}
	if AudienceScopeName("ns1", "agent-a") == AudienceScopeName("ns1", "agent-b") {
		t.Error("scope names must distinguish workloads; otherwise unregister cannot be per-workload")
	}
}

// TestAudienceScopeNameMatchesTheOperator pins the scope name against the
// operator's AudienceScopeName, including its lossy slash replacement.
func TestAudienceScopeNameMatchesTheOperator(t *testing.T) {
	for _, tc := range []struct{ ns, workload, want string }{
		{"ns1", "agent-a", "agent-ns1-agent-a-aud"},
		{"default", "my-agent", "agent-default-my-agent-aud"},
		// The operator replaces every slash, so a workload name containing one
		// collapses the same way. Reproduced deliberately: the target is whatever
		// name the operator actually created.
		{"a", "b/c", "agent-a-b-c-aud"},
	} {
		if got := AudienceScopeName(tc.ns, tc.workload); got != tc.want {
			t.Errorf("AudienceScopeName(%q, %q) = %q, want %q", tc.ns, tc.workload, got, tc.want)
		}
	}
}

// TestUnregisterRemovesScopeAndLinks is the main path: the scope, its realm-level
// entry, and its attachment on the platform client all go, and the client is left
// alone because --force was not given.
func TestUnregisterRemovesScopeAndLinks(t *testing.T) {
	f := newFakeKeycloak(t)
	f.scopes["agent-ns1-agent-a-aud"] = "scope-uuid"
	f.clients[testClientID] = "client-uuid"
	f.clients["rossoctl"] = "platform-uuid"

	res, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
		Realm:     testRealm,
		ClientID:  testClientID,
		ScopeName: "agent-ns1-agent-a-aud",
		Platforms: DefaultPlatformClientIDs,
	})
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}

	if !res.ScopeDeleted || !res.RealmLinkRemoved {
		t.Errorf("scope and realm link should be removed: %+v", res)
	}
	if !slices.Equal(res.PlatformUnlinked, []string{"rossoctl"}) {
		t.Errorf("PlatformUnlinked = %v, want [rossoctl]", res.PlatformUnlinked)
	}
	if res.ClientDeleted {
		t.Error("client must not be deleted without DeleteClient")
	}
	if !res.ClientSkipped {
		t.Error("an existing client left in place should be reported as skipped")
	}

	// The exact endpoints, which is what proves this is the inverse of the
	// operator's PUTs rather than merely something that returns success.
	for _, want := range []string{
		"DELETE /admin/realms/rossoctl/default-default-client-scopes/scope-uuid",
		"DELETE /admin/realms/rossoctl/clients/platform-uuid/default-client-scopes/scope-uuid",
		"DELETE /admin/realms/rossoctl/client-scopes/scope-uuid",
	} {
		if f.indexOf(want) < 0 {
			t.Errorf("missing request %q; got %v", want, f.requests)
		}
	}
	if got := f.indexOf("DELETE /admin/realms/rossoctl/clients/client-uuid"); got >= 0 {
		t.Errorf("client was deleted without --force (request %d)", got)
	}
}

// TestUnregisterUnlinksBeforeDeletingTheScope pins the ordering. Keycloak
// cascades attachment removal when a scope is deleted, so a reversed order would
// still pass the assertions above — but an interrupted run would leave the realm
// or a shared client pointing at a scope that no longer exists, which is the leak
// this command exists to fix.
func TestUnregisterUnlinksBeforeDeletingTheScope(t *testing.T) {
	f := newFakeKeycloak(t)
	f.scopes["s"] = "scope-uuid"
	f.clients["rossoctl"] = "platform-uuid"

	if _, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "s",
		Platforms: []string{"rossoctl"},
	}); err != nil {
		t.Fatalf("Unregister: %v", err)
	}

	scopeDelete := f.indexOf("DELETE /admin/realms/rossoctl/client-scopes/scope-uuid")
	realmUnlink := f.indexOf("DELETE /admin/realms/rossoctl/default-default-client-scopes/scope-uuid")
	platUnlink := f.indexOf("DELETE /admin/realms/rossoctl/clients/platform-uuid/default-client-scopes/scope-uuid")

	if scopeDelete < 0 || realmUnlink < 0 || platUnlink < 0 {
		t.Fatalf("expected all three deletes; got %v", f.requests)
	}
	if realmUnlink > scopeDelete {
		t.Error("realm link must be removed before the scope is deleted")
	}
	if platUnlink > scopeDelete {
		t.Error("platform link must be removed before the scope is deleted")
	}
}

// TestUnregisterForceDeletesTheClient verifies --force reaches the client, and
// that the client is deleted last: it is the step with consequences beyond this
// workload, so a failure earlier should stop short of it.
func TestUnregisterForceDeletesTheClient(t *testing.T) {
	f := newFakeKeycloak(t)
	f.scopes["s"] = "scope-uuid"
	f.clients[testClientID] = "client-uuid"

	res, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "s",
		DeleteClient: true,
	})
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if !res.ClientDeleted {
		t.Fatalf("client should be deleted with DeleteClient: %+v", res)
	}
	if res.ClientSkipped {
		t.Error("ClientSkipped must not be set when the client was deleted")
	}

	clientDelete := f.indexOf("DELETE /admin/realms/rossoctl/clients/client-uuid")
	scopeDelete := f.indexOf("DELETE /admin/realms/rossoctl/client-scopes/scope-uuid")
	if clientDelete < 0 {
		t.Fatalf("client was not deleted; got %v", f.requests)
	}
	if clientDelete < scopeDelete {
		t.Error("the client should be deleted after the scope, being the riskiest step")
	}
}

// TestUnregisterIsIdempotent verifies a second run succeeds. Cleanup a user may
// repeat, or run after a partial failure, must converge rather than fail on what
// is already gone.
func TestUnregisterIsIdempotent(t *testing.T) {
	f := newFakeKeycloak(t)
	// An empty realm: no scope, no client.
	res, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "gone",
		Platforms: DefaultPlatformClientIDs, DeleteClient: true,
	})
	if err != nil {
		t.Fatalf("Unregister on an empty realm should succeed: %v", err)
	}
	if !res.ScopeAbsent {
		t.Error("a missing scope should be reported as absent")
	}
	if !res.ClientAbsent {
		t.Error("a missing client should be reported as absent")
	}
	if res.ScopeDeleted || res.ClientDeleted {
		t.Errorf("nothing should be reported deleted: %+v", res)
	}
}

// TestUnregisterTreats404AsDone verifies a DELETE racing another caller is not an
// error: the object is gone, which is what was wanted.
func TestUnregisterTreats404AsDone(t *testing.T) {
	f := newFakeKeycloak(t)
	f.scopes["s"] = "scope-uuid"
	f.status["DELETE "] = http.StatusNotFound

	res, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "s",
	})
	if err != nil {
		t.Fatalf("a 404 from DELETE should not be an error: %v", err)
	}
	if !res.ScopeDeleted {
		t.Error("a scope that is already gone should still count as removed")
	}
}

// TestUnregisterReportsADeleteFailure verifies a real error is surfaced rather
// than swallowed. Cleanup that reports success while leaving objects behind is
// worse than cleanup that fails loudly.
func TestUnregisterReportsADeleteFailure(t *testing.T) {
	f := newFakeKeycloak(t)
	f.scopes["s"] = "scope-uuid"
	f.status["DELETE /admin/realms/rossoctl/client-scopes/"] = http.StatusInternalServerError

	_, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "s",
	})
	if err == nil {
		t.Fatal("a 500 from DELETE should be reported")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should name the status: %v", err)
	}
}

// TestUnregisterStopsBeforeDeletingTheClientOnFailure verifies an earlier failure
// prevents the irreversible step. The client is shared, so it must not be removed
// on the strength of a cleanup that did not complete.
func TestUnregisterStopsBeforeDeletingTheClientOnFailure(t *testing.T) {
	f := newFakeKeycloak(t)
	f.scopes["s"] = "scope-uuid"
	f.clients[testClientID] = "client-uuid"
	f.status["DELETE /admin/realms/rossoctl/client-scopes/"] = http.StatusInternalServerError

	if _, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "s", DeleteClient: true,
	}); err == nil {
		t.Fatal("expected the scope delete to fail")
	}
	if i := f.indexOf("DELETE /admin/realms/rossoctl/clients/client-uuid"); i >= 0 {
		t.Error("the client must not be deleted after an earlier step failed")
	}
}

// TestUnregisterSkipsAbsentPlatformClient verifies a platform client that does not
// exist is skipped rather than failing the run, matching the operator's own
// attachment loop: a deployment may not include that client.
func TestUnregisterSkipsAbsentPlatformClient(t *testing.T) {
	f := newFakeKeycloak(t)
	f.scopes["s"] = "scope-uuid"

	res, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "s",
		Platforms: []string{"rossoctl", "", "  "},
	})
	if err != nil {
		t.Fatalf("an absent platform client should not fail the run: %v", err)
	}
	if len(res.PlatformUnlinked) != 0 {
		t.Errorf("PlatformUnlinked = %v, want none", res.PlatformUnlinked)
	}
}

// TestFindClientRequiresAnExactMatch verifies the clientId query result is
// compared rather than trusted. Keycloak's search has matched substrings, and
// deleting a client whose id merely contains the requested one would be the worst
// outcome this command could produce.
func TestFindClientRequiresAnExactMatch(t *testing.T) {
	f := newFakeKeycloak(t)
	// The fake answers the filter with a different clientId, as a substring match
	// would.
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/clients"):
			writeJSON(w, []map[string]string{
				{"id": "other-uuid", "clientId": testClientID + "-suffix"},
			})
		case strings.HasSuffix(r.URL.Path, "/client-scopes"):
			writeJSON(w, []map[string]string{})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	res, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "none",
		DeleteClient: true,
	})
	if err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if res.ClientDeleted {
		t.Fatal("a client whose id only contains the requested one must not be deleted")
	}
	if !res.ClientAbsent {
		t.Error("no exact match means absent")
	}
}

// TestPasswordGrantTokenUsesTheMasterRealm verifies the admin grant targets the
// master realm with admin-cli, independent of the realm being modified. The admin
// account lives there whatever realm the workload is in.
func TestPasswordGrantTokenUsesTheMasterRealm(t *testing.T) {
	f := newFakeKeycloak(t)

	var form string
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		form = string(b)
		writeJSON(w, map[string]any{"access_token": "tok", "expires_in": 60})
	})

	got, err := f.client().PasswordGrantToken(context.Background(), "admin", "secret")
	if err != nil {
		t.Fatalf("PasswordGrantToken: %v", err)
	}
	if got != "tok" {
		t.Errorf("token = %q, want %q", got, "tok")
	}
	if want := "POST /realms/master/protocol/openid-connect/token"; f.indexOf(want) < 0 {
		t.Errorf("missing %q; got %v", want, f.requests)
	}
	for _, want := range []string{"grant_type=password", "client_id=admin-cli", "username=admin"} {
		if !strings.Contains(form, want) {
			t.Errorf("form %q should contain %q", form, want)
		}
	}
}

// TestPasswordGrantTokenReportsBadCredentials verifies a 401 is surfaced with the
// status, which is usually the only clue: Keycloak answers the same for a wrong
// password and for an account lacking admin rights.
func TestPasswordGrantTokenReportsBadCredentials(t *testing.T) {
	f := newFakeKeycloak(t)
	f.status["POST /realms/master"] = http.StatusUnauthorized

	_, err := f.client().PasswordGrantToken(context.Background(), "admin", "wrong")
	if err == nil {
		t.Fatal("a 401 should be reported")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should name the status: %v", err)
	}
}

// TestBaseURLTolerartesATrailingSlash verifies a URL given with a trailing slash
// produces the same endpoints, matching the operator's trimBaseURL. A user pasting
// a browser URL should not get //admin paths.
func TestBaseURLToleratesATrailingSlash(t *testing.T) {
	f := newFakeKeycloak(t)
	f.scopes["s"] = "scope-uuid"

	c := &Client{BaseURL: f.srv.URL + "/"}
	if _, err := c.Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "s",
	}); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	for _, got := range f.requests {
		if strings.Contains(got, "//admin") {
			t.Errorf("request %q has a doubled slash", got)
		}
	}
}

// TestUnregisterRejectsATraversingRealm verifies a realm carrying dot segments is
// refused before any request is sent.
//
// This is not theoretical escaping pedantry: url.PathEscape leaves ".." intact, so
// "../master" yields /admin/realms/../master/clients. This code never resolves
// that, but a server or proxy may normalize it and retarget the request at a realm
// the caller never named — and every request here is a DELETE.
func TestUnregisterRejectsATraversingRealm(t *testing.T) {
	for _, realm := range []string{"../master", "..", ".", "a/b", `a\b`, "", "  ", " rossoctl"} {
		f := newFakeKeycloak(t)
		_, err := f.client().Unregister(context.Background(), "tok", UnregisterOptions{
			Realm: realm, ClientID: testClientID, ScopeName: "s", DeleteClient: true,
		})
		if err == nil {
			t.Errorf("realm %q should be rejected", realm)
		}
		// Rejected before the wire, not after a request went out.
		if len(f.requests) != 0 {
			t.Errorf("realm %q produced requests %v; it should be refused first", realm, f.requests)
		}
	}
}

// TestValidateRealmAcceptsRealNames verifies the check does not reject the names
// an installation actually uses. A validator that refuses valid input is its own
// bug.
func TestValidateRealmAcceptsRealNames(t *testing.T) {
	for _, realm := range []string{"rossoctl", "master", "my-realm", "realm_1", "Realm.2"} {
		if err := ValidateRealm(realm); err != nil {
			t.Errorf("ValidateRealm(%q) = %v, want nil", realm, err)
		}
	}
}

// TestLogfReceivesOneLinePerObject verifies the verbose seam reports each step,
// so --verbose output reflects the work rather than being generated separately.
func TestLogfReceivesOneLinePerObject(t *testing.T) {
	f := newFakeKeycloak(t)
	f.scopes["s"] = "scope-uuid"
	f.clients[testClientID] = "client-uuid"
	f.clients["rossoctl"] = "platform-uuid"

	var lines []string
	c := f.client()
	c.Logf = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	if _, err := c.Unregister(context.Background(), "tok", UnregisterOptions{
		Realm: testRealm, ClientID: testClientID, ScopeName: "s",
		Platforms: DefaultPlatformClientIDs,
	}); err != nil {
		t.Fatalf("Unregister: %v", err)
	}

	joined := strings.Join(lines, "\n")
	for _, want := range []string{"realm default-default", "platform client", "deleted client scope", "left in place"} {
		if !strings.Contains(joined, want) {
			t.Errorf("verbose output should mention %q; got:\n%s", want, joined)
		}
	}
}
