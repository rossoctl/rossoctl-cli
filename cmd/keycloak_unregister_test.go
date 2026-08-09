package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeKeycloakServer serves the admin endpoints the keycloak commands use, recording
// the requests so a test can assert what the command actually asked Keycloak to do.
//
// The realm starts out populated: a scope named by scopeName and both the workload
// client and the platform client. A test that wants an empty realm passes "".
func fakeKeycloakServer(t *testing.T, scopeName, clientID string) (*httptest.Server, *[]string) {
	t.Helper()
	var requests []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"access_token":"admin-token","expires_in":60}`))

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/clients"):
			want := r.URL.Query().Get("clientId")
			if want == clientID || want == "rossoctl" {
				_, _ = w.Write([]byte(`[{"id":"uuid-` + want + `","clientId":"` + want + `"}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/client-scopes"):
			if scopeName == "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":"scope-uuid","name":"` + scopeName + `"}]`))

		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// sawRequest reports whether the recorded requests include one containing sub.
func sawRequest(requests []string, sub string) bool {
	for _, r := range requests {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

// TestKeycloakUnregisterRequiresWorkload verifies the one required flag is enforced,
// and that nothing is contacted without it. A cleanup command that guesses at its
// target would delete the wrong objects.
func TestKeycloakUnregisterRequiresWorkload(t *testing.T) {
	srv, requests := fakeKeycloakServer(t, "any", "any")

	out, err := execute(t, "keycloak", "unregister", "--keycloakURL", srv.URL, "--namespace", "ns1")
	if err == nil {
		t.Fatalf("omitting --workload should fail; output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--workload") {
		t.Errorf("error should name the missing flag: %v", err)
	}
	if len(*requests) != 0 {
		t.Errorf("nothing should be contacted without --workload; got %v", *requests)
	}
}

// TestKeycloakUnregisterDeletesScopeAndItsLinks verifies the default run: the
// per-workload scope and both its attachments go, and the derived names are the
// ones the operator would have created.
func TestKeycloakUnregisterDeletesScopeAndItsLinks(t *testing.T) {
	const clientID = "spiffe://localtest.me/ns/ns1/sa/agent-sa"
	srv, requests := fakeKeycloakServer(t, "agent-ns1-agent-a-aud", clientID)

	out, err := execute(t, "keycloak", "unregister",
		"--keycloakURL", srv.URL, "--namespace", "ns1",
		"--sa", "agent-sa", "--workload", "agent-a")
	if err != nil {
		t.Fatalf("unregister: %v\n%s", err, out)
	}

	// The scope name the operator derives from "<namespace>/<workload>".
	if !strings.Contains(out, "agent-ns1-agent-a-aud") {
		t.Errorf("output should name the scope it deleted:\n%s", out)
	}
	for _, want := range []string{
		"DELETE /admin/realms/rossoctl/default-default-client-scopes/scope-uuid",
		"DELETE /admin/realms/rossoctl/clients/uuid-rossoctl/default-client-scopes/scope-uuid",
		"DELETE /admin/realms/rossoctl/client-scopes/scope-uuid",
	} {
		if !sawRequest(*requests, want) {
			t.Errorf("missing %q; got %v", want, *requests)
		}
	}
}

// TestKeycloakUnregisterKeepsTheClientByDefault verifies the client survives a run
// without --force, and that the output says so.
//
// This is the command's most consequential default: the client is keyed on the
// ServiceAccount, so deleting it would break sibling workloads. Silence would let
// a user assume it was removed.
func TestKeycloakUnregisterKeepsTheClientByDefault(t *testing.T) {
	const clientID = "spiffe://localtest.me/ns/ns1/sa/agent-sa"
	srv, requests := fakeKeycloakServer(t, "agent-ns1-agent-a-aud", clientID)

	out, err := execute(t, "keycloak", "unregister",
		"--keycloakURL", srv.URL, "--namespace", "ns1",
		"--sa", "agent-sa", "--workload", "agent-a")
	if err != nil {
		t.Fatalf("unregister: %v\n%s", err, out)
	}

	if sawRequest(*requests, "DELETE /admin/realms/rossoctl/clients/uuid-spiffe") {
		t.Error("the client must not be deleted without --force")
	}
	if !strings.Contains(out, "NOT deleted") {
		t.Errorf("output should state the client was kept:\n%s", out)
	}
	if !strings.Contains(out, "--force") {
		t.Errorf("output should say how to delete it:\n%s", out)
	}
}

// TestKeycloakUnregisterForceDeletesTheClient verifies --force removes the client, and
// that the SPIFFE ID it targets is the operator's format.
func TestKeycloakUnregisterForceDeletesTheClient(t *testing.T) {
	const clientID = "spiffe://localtest.me/ns/ns1/sa/agent-sa"
	srv, requests := fakeKeycloakServer(t, "agent-ns1-agent-a-aud", clientID)

	out, err := execute(t, "keycloak", "unregister",
		"--keycloakURL", srv.URL, "--namespace", "ns1",
		"--sa", "agent-sa", "--workload", "agent-a", "--force")
	if err != nil {
		t.Fatalf("unregister: %v\n%s", err, out)
	}

	if !sawRequest(*requests, "DELETE /admin/realms/rossoctl/clients/uuid-"+clientID) {
		t.Errorf("client should be deleted with --force; got %v", *requests)
	}
	if !strings.Contains(out, clientID) {
		t.Errorf("output should name the client it deleted:\n%s", out)
	}
}

// TestKeycloakUnregisterDefaultsTheServiceAccount verifies --sa defaults to "default",
// which is the ServiceAccount name the operator substitutes for an unset one.
func TestKeycloakUnregisterDefaultsTheServiceAccount(t *testing.T) {
	const clientID = "spiffe://localtest.me/ns/ns1/sa/default"
	srv, _ := fakeKeycloakServer(t, "agent-ns1-agent-a-aud", clientID)

	out, err := execute(t, "keycloak", "unregister",
		"--keycloakURL", srv.URL, "--namespace", "ns1",
		"--workload", "agent-a", "--force")
	if err != nil {
		t.Fatalf("unregister: %v\n%s", err, out)
	}
	if !strings.Contains(out, "/sa/default") {
		t.Errorf("the client ID should use the default ServiceAccount:\n%s", out)
	}
}

// TestKeycloakUnregisterUsesTheContextNamespace verifies --namespace defaults to the
// current context's namespace, as the flag's documentation promises.
func TestKeycloakUnregisterUsesTheContextNamespace(t *testing.T) {
	isolateHome(t)

	// Seeded through the CLI's own command, so the test depends on the context
	// format the code writes rather than on a hand-built file.
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://unused.example/api/v1/", "--namespace", "team7"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	const clientID = "spiffe://localtest.me/ns/team7/sa/default"
	srv, _ := fakeKeycloakServer(t, "agent-team7-agent-a-aud", clientID)

	out, err := execute(t, "keycloak", "unregister",
		"--keycloakURL", srv.URL, "--workload", "agent-a", "--force")
	if err != nil {
		t.Fatalf("unregister: %v\n%s", err, out)
	}
	if !strings.Contains(out, "/ns/team7/") {
		t.Errorf("the context's namespace should be used:\n%s", out)
	}
	if !strings.Contains(out, "agent-team7-agent-a-aud") {
		t.Errorf("the scope name should use the context's namespace:\n%s", out)
	}
}

// TestKeycloakUnregisterReportsAnAbsentScope verifies a realm with nothing to remove
// says so rather than reporting success for work that did not happen.
func TestKeycloakUnregisterReportsAnAbsentScope(t *testing.T) {
	srv, _ := fakeKeycloakServer(t, "", "none")

	out, err := execute(t, "keycloak", "unregister",
		"--keycloakURL", srv.URL, "--namespace", "ns1", "--workload", "ghost")
	if err != nil {
		t.Fatalf("an already-clean realm should not be an error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("output should report what was absent:\n%s", out)
	}
}

// TestKeycloakUnregisterRejectsABadRealm verifies a realm that would retarget the URL
// path is refused before any request is sent.
func TestKeycloakUnregisterRejectsABadRealm(t *testing.T) {
	srv, requests := fakeKeycloakServer(t, "s", "c")

	out, err := execute(t, "keycloak", "unregister",
		"--keycloakURL", srv.URL, "--realm", "../master",
		"--namespace", "ns1", "--workload", "agent-a")
	if err == nil {
		t.Fatalf("a traversing realm should be rejected; output:\n%s", out)
	}
	if len(*requests) != 0 {
		t.Errorf("nothing should be contacted with a bad realm; got %v", *requests)
	}
}

// TestKeycloakUnregisterReportsABadPassword verifies an admin credential failure is
// surfaced, since the flags default to the development password and a real
// installation will not share it.
func TestKeycloakUnregisterReportsABadPassword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := execute(t, "keycloak", "unregister",
		"--keycloakURL", srv.URL, "--namespace", "ns1", "--workload", "agent-a")
	if err == nil {
		t.Fatal("a 401 from the token endpoint should fail the command")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should name the status: %v", err)
	}
}

// TestKeycloakFlagDefaults pins the defaults, which are part of the requested
// interface rather than incidental.
func TestKeycloakFlagDefaults(t *testing.T) {
	resetFlags(rootCmd)

	kc, _, err := rootCmd.Find([]string{"keycloak"})
	if err != nil {
		t.Fatalf("find keycloak: %v", err)
	}
	for _, tc := range []struct{ flag, want string }{
		{"keycloakURL", "http://keycloak.localtest.me:8080"},
		{"adminUser", "admin"},
		{"adminPass", "admin"},
		{"realm", "rossoctl"},
	} {
		f := kc.PersistentFlags().Lookup(tc.flag)
		if f == nil {
			t.Errorf("keycloak has no --%s flag", tc.flag)
			continue
		}
		if f.DefValue != tc.want {
			t.Errorf("--%s default = %q, want %q", tc.flag, f.DefValue, tc.want)
		}
	}

	unreg, _, err := rootCmd.Find([]string{"keycloak", "unregister"})
	if err != nil {
		t.Fatalf("find keycloak unregister: %v", err)
	}
	for _, tc := range []struct{ flag, want string }{
		{"sa", "default"},
		{"trustDomain", "localtest.me"},
		{"namespace", ""},
		{"workload", ""},
		{"force", "false"},
	} {
		f := unreg.Flags().Lookup(tc.flag)
		if f == nil {
			t.Errorf("keycloak unregister has no --%s flag", tc.flag)
			continue
		}
		if f.DefValue != tc.want {
			t.Errorf("--%s default = %q, want %q", tc.flag, f.DefValue, tc.want)
		}
	}
}
