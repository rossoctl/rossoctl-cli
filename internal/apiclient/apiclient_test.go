package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetAuthConfig(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"enabled": true,
			"keycloak_url": "https://kc.example.com",
			"realm": "rossoctl",
			"client_id": "rossoctl-ui",
			"redirect_uri": null
		}`))
	}))
	defer srv.Close()

	// Base URL includes an /api/v1/ prefix, like the real default, to ensure
	// the endpoint path is appended rather than replacing the prefix.
	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	cfg, err := c.GetAuthConfig(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/api/v1/auth/config" {
		t.Errorf("requested path = %q, want %q", gotPath, "/api/v1/auth/config")
	}
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.KeycloakURL == nil || *cfg.KeycloakURL != "https://kc.example.com" {
		t.Errorf("KeycloakURL = %v, want https://kc.example.com", cfg.KeycloakURL)
	}
	if cfg.RedirectURI != nil {
		t.Errorf("RedirectURI = %v, want nil", cfg.RedirectURI)
	}
}

func TestGetAuthConfigBaseWithoutTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/config" {
			t.Errorf("requested path = %q, want %q", r.URL.Path, "/api/v1/auth/config")
		}
		_, _ = w.Write([]byte(`{"enabled": false}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1"} // no trailing slash
	if _, err := c.GetAuthConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListTools(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"items":[
			{"name":"weather-mcp","namespace":"team1","description":"d","status":"Ready",
			 "labels":{"protocol":["mcp"],"framework":null,"type":"tool"},
			 "workloadType":"deployment","createdAt":null}
		]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	resp, err := c.ListTools(context.Background(), "team1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/api/v1/tools" {
		t.Errorf("path = %q, want %q", gotPath, "/api/v1/tools")
	}
	if gotQuery != "namespace=team1" {
		t.Errorf("query = %q, want %q", gotQuery, "namespace=team1")
	}
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(resp.Items))
	}
	tl := resp.Items[0]
	if tl.Name != "weather-mcp" || tl.Status != "Ready" {
		t.Errorf("unexpected tool: %+v", tl)
	}
	if len(tl.Labels.Protocol) != 1 || tl.Labels.Protocol[0] != "mcp" {
		t.Errorf("protocol = %v, want [mcp]", tl.Labels.Protocol)
	}
}

func TestDeleteTool(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"success": true, "message": "deleted"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	resp, err := c.DeleteTool(context.Background(), "team1", "weather-mcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/v1/tools/team1/weather-mcp" {
		t.Errorf("path = %q, want /api/v1/tools/team1/weather-mcp", gotPath)
	}
	if !resp.Success || resp.Message != "deleted" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestCreateTool(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"success": true, "name": "weather-mcp", "namespace": "team1", "message": "Tool created"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	req := &CreateToolRequest{
		Name:             "weather-mcp",
		Namespace:        "team1",
		DeploymentMethod: "image",
		WorkloadType:     "deployment",
		ContainerImage:   "ghcr.io/x/y:latest",
		ImagePullSecret:  "regcred",
		EnvVars:          []EnvVar{{Name: "FOO", Value: "bar"}},
	}
	resp, err := c.CreateTool(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/tools" {
		t.Errorf("path = %q, want /api/v1/tools", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}
	if gotBody["deploymentMethod"] != "image" || gotBody["containerImage"] != "ghcr.io/x/y:latest" {
		t.Errorf("unexpected body: %+v", gotBody)
	}
	envVars, ok := gotBody["envVars"].([]any)
	if !ok || len(envVars) != 1 {
		t.Fatalf("envVars not sent correctly: %+v", gotBody["envVars"])
	}
	if !resp.Success || resp.Message != "Tool created" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestCreateToolOmitsEmptyOptionals(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"success": true}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	_, err := c.CreateTool(context.Background(), &CreateToolRequest{
		Name: "a", Namespace: "team1", DeploymentMethod: "image", WorkloadType: "deployment",
		ContainerImage: "img",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, k := range []string{"imagePullSecret", "gitUrl", "gitPath", "gitBranch", "envVars"} {
		if _, present := gotBody[k]; present {
			t.Errorf("empty field %q should be omitted, body: %+v", k, gotBody)
		}
	}
}

func TestListAgents(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"items":[
			{"name":"a","namespace":"team1","description":"d","status":"Ready",
			 "labels":{"protocol":["a2a"],"framework":"LangGraph","type":"agent"},
			 "workloadType":"deployment","createdAt":"2026-01-01T00:00:00Z"}
		]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	resp, err := c.ListAgents(context.Background(), "team1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/api/v1/agents" {
		t.Errorf("path = %q, want %q", gotPath, "/api/v1/agents")
	}
	if gotQuery != "namespace=team1" {
		t.Errorf("query = %q, want %q", gotQuery, "namespace=team1")
	}
	if len(resp.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(resp.Items))
	}
	a := resp.Items[0]
	if a.Name != "a" || a.Status != "Ready" {
		t.Errorf("unexpected agent: %+v", a)
	}
	if len(a.Labels.Protocol) != 1 || a.Labels.Protocol[0] != "a2a" {
		t.Errorf("protocol = %v, want [a2a]", a.Labels.Protocol)
	}
}

func TestListAgentsNoNamespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("expected no query string, got %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	if _, err := c.ListAgents(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetAgent(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"metadata": {"name":"orders","namespace":"team1","uid":"u1"},
			"spec": {"replicas": 3, "source": {"git": {"url": "http://g"}}},
			"status": {"conditions": [{"type":"Available","status":"True"}]},
			"workloadType": "deployment",
			"readyStatus": "Ready",
			"service": {"name":"orders","type":"ClusterIP","clusterIP":"1.2.3.4","ports":[{"name":"http","port":8080,"targetPort":8000}]}
		}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	agent, err := c.GetAgent(context.Background(), "team1", "orders")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/api/v1/agents/team1/orders" {
		t.Errorf("path = %q, want %q", gotPath, "/api/v1/agents/team1/orders")
	}
	if agent.Metadata.Name != "orders" || agent.ReadyStatus != "Ready" {
		t.Errorf("unexpected agent: %+v", agent.Metadata)
	}
	if agent.Service == nil || agent.Service.ClusterIP != "1.2.3.4" {
		t.Errorf("service not decoded: %+v", agent.Service)
	}
	if _, ok := agent.Spec["source"]; !ok {
		t.Error("spec.source not present in decoded map")
	}
}

func TestGetTool(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"metadata": {"name":"weather-mcp","namespace":"team1","uid":"u9"},
			"spec": {"replicas": 1},
			"status": {"conditions": [{"type":"Available","status":"True"}]},
			"workloadType": "deployment",
			"readyStatus": "Ready",
			"service": {"name":"weather-mcp","type":"ClusterIP","clusterIP":"5.6.7.8","ports":[{"name":"mcp","port":8000,"targetPort":8000}]}
		}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	tool, err := c.GetTool(context.Background(), "team1", "weather-mcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/api/v1/tools/team1/weather-mcp" {
		t.Errorf("path = %q, want %q", gotPath, "/api/v1/tools/team1/weather-mcp")
	}
	if tool.Metadata.Name != "weather-mcp" || tool.ReadyStatus != "Ready" {
		t.Errorf("unexpected tool: %+v", tool.Metadata)
	}
	if tool.Service == nil || tool.Service.ClusterIP != "5.6.7.8" {
		t.Errorf("service not decoded: %+v", tool.Service)
	}
}

func TestDeleteAgent(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"success": true, "message": "deleted"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	resp, err := c.DeleteAgent(context.Background(), "team1", "orders")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/api/v1/agents/team1/orders" {
		t.Errorf("path = %q, want /api/v1/agents/team1/orders", gotPath)
	}
	if !resp.Success || resp.Message != "deleted" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestCreateAgent(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"success": true, "name": "orders", "namespace": "team1", "message": "Agent created"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	req := &CreateAgentRequest{
		Name:             "orders",
		Namespace:        "team1",
		DeploymentMethod: "image",
		WorkloadType:     "deployment",
		ContainerImage:   "ghcr.io/x/y:latest",
		ImagePullSecret:  "regcred",
		EnvVars:          []EnvVar{{Name: "FOO", Value: "bar"}},
	}
	resp, err := c.CreateAgent(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/agents" {
		t.Errorf("path = %q, want /api/v1/agents", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}
	if gotBody["deploymentMethod"] != "image" || gotBody["containerImage"] != "ghcr.io/x/y:latest" {
		t.Errorf("unexpected body: %+v", gotBody)
	}
	envVars, ok := gotBody["envVars"].([]any)
	if !ok || len(envVars) != 1 {
		t.Fatalf("envVars not sent correctly: %+v", gotBody["envVars"])
	}
	ev := envVars[0].(map[string]any)
	if ev["name"] != "FOO" || ev["value"] != "bar" {
		t.Errorf("envVar = %+v, want {FOO:bar}", ev)
	}
	if !resp.Success || resp.Message != "Agent created" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestCreateAgentOmitsEmptyOptionals(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"success": true}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	_, err := c.CreateAgent(context.Background(), &CreateAgentRequest{
		Name: "a", Namespace: "team1", DeploymentMethod: "image", WorkloadType: "deployment",
		ContainerImage: "img",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty optionals must be omitted (omitempty) so they don't override server defaults.
	for _, k := range []string{"imagePullSecret", "gitUrl", "gitPath", "gitBranch", "envVars"} {
		if _, present := gotBody[k]; present {
			t.Errorf("empty field %q should be omitted, body: %+v", k, gotBody)
		}
	}
}

func TestListNamespaces(t *testing.T) {
	tests := []struct {
		name        string
		enabledOnly bool
		wantQuery   string
	}{
		{name: "enabled only (default)", enabledOnly: true, wantQuery: ""},
		{name: "all", enabledOnly: false, wantQuery: "enabled_only=false"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery
				_, _ = w.Write([]byte(`{"namespaces":["default","team1"]}`))
			}))
			defer srv.Close()

			c := &Client{BaseURL: srv.URL + "/api/v1/"}
			resp, err := c.ListNamespaces(context.Background(), tt.enabledOnly)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotPath != "/api/v1/namespaces" {
				t.Errorf("path = %q, want %q", gotPath, "/api/v1/namespaces")
			}
			if gotQuery != tt.wantQuery {
				t.Errorf("query = %q, want %q", gotQuery, tt.wantQuery)
			}
			if len(resp.Namespaces) != 2 {
				t.Errorf("got %d namespaces, want 2", len(resp.Namespaces))
			}
		})
	}
}

func TestLogfCalledPerRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"enabled": false}`))
	}))
	defer srv.Close()

	var logs []string
	c := &Client{
		BaseURL: srv.URL + "/api/v1/",
		Logf:    func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	if _, err := c.GetAuthConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(logs) != 2 {
		t.Fatalf("expected 2 log lines (request + response), got %d: %v", len(logs), logs)
	}
	if !strings.HasPrefix(logs[0], "GET "+srv.URL+"/api/v1/auth/config") {
		t.Errorf("first log = %q, want a GET request line", logs[0])
	}
	if !strings.Contains(logs[1], "200 OK") {
		t.Errorf("second log = %q, want response status", logs[1])
	}
}

func TestBearerTokenHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"enabled": false}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/", BearerToken: "sekret"}
	if _, err := c.GetAuthConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sekret")
	}
}

func TestNoBearerTokenNoHeader(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		_, _ = w.Write([]byte(`{"enabled": false}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"} // no token
	if _, err := c.GetAuthConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hadAuth {
		t.Error("Authorization header sent when no BearerToken set")
	}
}

func TestNilLogfIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"enabled": false}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"} // Logf nil
	if _, err := c.GetAuthConfig(context.Background()); err != nil {
		t.Fatalf("unexpected error with nil Logf: %v", err)
	}
}

func TestGetAuthConfigServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	if _, err := c.GetAuthConfig(context.Background()); err == nil {
		t.Fatal("expected an error on 500, got nil")
	}
}

func TestGetAuthStatus(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"enabled": true, "authenticated": true, "keycloak_url": "https://kc", "realm": "rossoctl", "client_id": "rossoctl-ui"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	status, err := c.GetAuthStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/v1/auth/status" {
		t.Errorf("path = %q, want /api/v1/auth/status", gotPath)
	}
	if !status.Enabled || !status.Authenticated {
		t.Errorf("unexpected status: %+v", status)
	}
	if status.Realm == nil || *status.Realm != "rossoctl" {
		t.Errorf("realm = %v, want rossoctl", status.Realm)
	}
}

func TestGetUserInfo(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"username": "alice", "email": "a@x", "roles": ["admin"], "authenticated": true}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	info, err := c.GetUserInfo(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/v1/auth/me" {
		t.Errorf("path = %q, want /api/v1/auth/me", gotPath)
	}
	if info.Username != "alice" || !info.Authenticated || len(info.Roles) != 1 {
		t.Errorf("unexpected user info: %+v", info)
	}
}

func TestGetPlatformStatus(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"components": [{"name": "Istio", "status": "Ready"}],
			"registry": {"clusterBuildStrategyPresent": true, "clusterBuildStrategies": ["buildah"], "registryEndpoint": "r:5000"}
		}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	status, err := c.GetPlatformStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/api/v1/config/platform-status" {
		t.Errorf("path = %q, want /api/v1/config/platform-status", gotPath)
	}
	if len(status.Components) != 1 || status.Components[0].Name != "Istio" {
		t.Errorf("unexpected components: %+v", status.Components)
	}
	if !status.Registry.ClusterBuildStrategyPresent || status.Registry.RegistryEndpoint != "r:5000" {
		t.Errorf("unexpected registry: %+v", status.Registry)
	}
}

// TestStatusErrorIsReturnedForNon2xx verifies a failing response yields a
// *StatusError carrying the code, which is what lets the command layer suggest
// signing in on a 401 without matching on message text.
func TestStatusErrorIsReturnedForNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"detail":"Token signing key not found"}`)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	_, err := c.ListAgents(context.Background(), "team1")
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}

	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %T is not a *StatusError; the command layer could not detect the 401", err)
	}
	if statusErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", statusErr.StatusCode, http.StatusUnauthorized)
	}
	if !strings.Contains(statusErr.Body, "Token signing key not found") {
		t.Errorf("Body = %q, want the server's detail", statusErr.Body)
	}

	// The message is unchanged from the fmt.Errorf this replaced, so existing
	// output stays as it was and the hint is purely additive.
	want := fmt.Sprintf("%s/api/v1/agents?namespace=team1 returned 401: {\"detail\":\"Token signing key not found\"}", srv.URL)
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestStatusErrorUsesStatusLineWhenBodyEmpty verifies an empty body falls back to
// the status line, so the error is never just a bare code with nothing after it.
func TestStatusErrorUsesStatusLineWhenBodyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	_, err := c.ListAgents(context.Background(), "team1")

	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %T is not a *StatusError", err)
	}
	if statusErr.Body == "" {
		t.Error("Body is empty; want the status line as a fallback")
	}
}

// TestCreateRequestAdditionalParametersOverlay verifies AdditionalParameters are
// merged into the encoded request as top-level members, winning over the struct's
// own fields.
//
// Both request types are covered in one table because their marshalers differ only
// in which struct is encoded.
func TestCreateRequestAdditionalParametersOverlay(t *testing.T) {
	additional := map[string]any{
		// Replaces a field the struct populates.
		"containerImage": "override:1",
		// Sets a field tagged omitempty, which the struct left absent.
		"gitBranch": "release",
		// Replaces a bool that has no omitempty, so it is present either way.
		"createHttpRoute": true,
		// A member the struct has no field for at all.
		"serviceAccount": "sa",
	}

	for _, tc := range []struct {
		name string
		req  any
	}{
		{"agent", CreateAgentRequest{
			Name: "a", Namespace: "team1", DeploymentMethod: "image", WorkloadType: "deployment",
			ContainerImage: "img", AdditionalParameters: additional,
		}},
		{"tool", CreateToolRequest{
			Name: "a", Namespace: "team1", DeploymentMethod: "image", WorkloadType: "deployment",
			ContainerImage: "img", AdditionalParameters: additional,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if got["containerImage"] != "override:1" {
				t.Errorf("containerImage = %v, want the overlay to win", got["containerImage"])
			}
			if got["gitBranch"] != "release" {
				t.Errorf("gitBranch = %v, want release (an omitted field can be set)", got["gitBranch"])
			}
			if got["createHttpRoute"] != true {
				t.Errorf("createHttpRoute = %v, want true", got["createHttpRoute"])
			}
			if got["serviceAccount"] != "sa" {
				t.Errorf("serviceAccount = %v, want sa", got["serviceAccount"])
			}
			// Fields the overlay did not name are untouched.
			if got["name"] != "a" || got["workloadType"] != "deployment" {
				t.Errorf("unrelated fields changed: %+v", got)
			}
			// The map itself must never appear as a member: it is tagged json:"-",
			// and a nested "AdditionalParameters" object would be a field no server
			// knows.
			if _, present := got["AdditionalParameters"]; present {
				t.Errorf("the overlay map leaked in as a member: %+v", got)
			}
		})
	}
}

// TestCreateRequestWithoutAdditionalParametersIsUnchanged verifies a request that
// does not use the feature encodes exactly as it did before it existed.
//
// Byte comparison against the same struct marshaled through the alias type is the
// assertion: it catches the overlay path being taken for an empty map, which would
// reorder keys and could add a member, and would break a server that rejects
// unknown fields for callers who never asked for any of this.
func TestCreateRequestWithoutAdditionalParametersIsUnchanged(t *testing.T) {
	agent := CreateAgentRequest{
		Name: "a", Namespace: "team1", DeploymentMethod: "image", WorkloadType: "deployment",
		ContainerImage: "img", EnvVars: []EnvVar{{Name: "FOO", Value: "bar"}},
	}
	type plainAgent CreateAgentRequest
	want, err := json.Marshal(plainAgent(agent))
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	got, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("encoding changed with no additional parameters:\n got %s\nwant %s", got, want)
	}

	tool := CreateToolRequest{
		Name: "a", Namespace: "team1", DeploymentMethod: "image", WorkloadType: "deployment",
		ContainerImage: "img", ServicePorts: []CreateServicePort{{Name: "http", Port: 1, TargetPort: 1, Protocol: "TCP"}},
	}
	type plainTool CreateToolRequest
	wantTool, err := json.Marshal(plainTool(tool))
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	gotTool, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(gotTool) != string(wantTool) {
		t.Errorf("encoding changed with no additional parameters:\n got %s\nwant %s", gotTool, wantTool)
	}
}

// TestCreateRequestAdditionalParametersThroughPointer verifies the overlay applies
// when the request is marshaled as a pointer, which is how both client methods
// send it.
//
// Not redundant with the value-receiver tests above: a value-receiver MarshalJSON
// is in a pointer's method set, but the reverse is not true, so a marshaler
// written on *CreateAgentRequest would silently do nothing for a value and one on
// the value works for both. This pins the direction the callers actually use.
func TestCreateRequestAdditionalParametersThroughPointer(t *testing.T) {
	data, err := json.Marshal(&CreateAgentRequest{
		Name: "a", ContainerImage: "img",
		AdditionalParameters: map[string]any{"containerImage": "override:1"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["containerImage"] != "override:1" {
		t.Errorf("containerImage = %v, want the overlay applied through the pointer", got["containerImage"])
	}
}

// TestCreateAgentSendsAdditionalParameters verifies the overlay survives the real
// POST path, not just a direct json.Marshal.
func TestCreateAgentSendsAdditionalParameters(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"success": true}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	if _, err := c.CreateAgent(context.Background(), &CreateAgentRequest{
		Name: "a", Namespace: "team1", DeploymentMethod: "image", WorkloadType: "deployment",
		ContainerImage:       "img",
		AdditionalParameters: map[string]any{"replicas": 3, "containerImage": "override:1"},
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	if gotBody["replicas"] != float64(3) {
		t.Errorf("replicas = %#v, want 3", gotBody["replicas"])
	}
	if gotBody["containerImage"] != "override:1" {
		t.Errorf("containerImage = %v, want the overlay to win on the wire", gotBody["containerImage"])
	}
}
