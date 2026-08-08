package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// agentDetailBody mimics the backend GET /agents/{ns}/{name} response for a
// source-built deployment agent with a service and status conditions.
const agentDetailBody = `{
	"metadata": {
		"name": "orders",
		"namespace": "team1",
		"labels": {"protocol.rossoctl.io/a2a": "true", "rossoctl.io/workload-type": "deployment"},
		"annotations": {"rossoctl.io/description": "Handles orders"},
		"creationTimestamp": "2026-01-02T03:04:05Z",
		"uid": "abc-123"
	},
	"spec": {
		"replicas": 2,
		"source": {"git": {"url": "https://github.com/x/y", "path": "agents/orders", "branch": "dev"}},
		"image": {"tag": "v0.0.1"}
	},
	"status": {
		"readyReplicas": 2,
		"availableReplicas": 2,
		"conditions": [
			{"type": "Available", "status": "True", "reason": "MinimumReplicasAvailable",
			 "message": "Deployment has minimum availability.", "lastTransitionTime": "2026-01-02T03:05:00Z"}
		]
	},
	"workloadType": "deployment",
	"readyStatus": "Ready",
	"service": {
		"name": "orders",
		"type": "ClusterIP",
		"clusterIP": "10.0.0.5",
		"ports": [{"name": "http", "port": 8080, "targetPort": 8000}]
	}
}`

// setupAgentGetContext points the current context at srv with namespace team1.
func setupAgentGetContext(t *testing.T, srv *httptest.Server) {
	t.Helper()
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	// set-context validates against /namespaces; the mock returns team1.
	if _, err := execute(t, "config", "set-context", "--namespace", "team1"); err != nil {
		t.Fatalf("set-context: %v", err)
	}
}

func newAgentGetServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case "/api/v1/agents/team1/orders":
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte(body))
		case "/api/v1/agents/team1/orders/route-status":
			// `agents get` reports the external route, so it asks for this too.
			_, _ = w.Write([]byte(`{"hasRoute":true}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAgentsGetText(t *testing.T) {
	isolateHome(t)
	srv := newAgentGetServer(t, agentDetailBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "get", "orders")
	if err != nil {
		t.Fatalf("agents get: %v", err)
	}

	// Section headers and key fields, mirroring the UI's detail page.
	for _, want := range []string{
		"orders",
		"Status: Ready",
		"Protocols: A2A",
		"Agent Information",
		"Namespace:", "team1",
		"Description:", "Handles orders",
		"Workload Type:", "Deployment",
		"Replicas:", "2/2 ready (2 available)",
		"Created:", "2026-01-02T03:04:05Z",
		"UID:", "abc-123",
		"Endpoint",
		"Service:", "orders (ClusterIP)",
		"Cluster IP:", "10.0.0.5",
		"Ports:", "http: 8080→8000",
		"Source",
		"Git URL:", "https://github.com/x/y",
		"Path:", "agents/orders",
		"Branch:", "dev",
		"Image Tag:", "v0.0.1",
		"Status",
		"Available", "True", "MinimumReplicasAvailable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}

	// It must not be raw JSON.
	if strings.Contains(out, "\"metadata\"") {
		t.Errorf("text output unexpectedly contains raw JSON:\n%s", out)
	}
}

func TestAgentsGetJSON(t *testing.T) {
	isolateHome(t)
	srv := newAgentGetServer(t, agentDetailBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "get", "orders", "--json")
	if err != nil {
		t.Fatalf("agents get --json: %v", err)
	}
	var decoded struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		ReadyStatus string `json:"readyStatus"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if decoded.Metadata.Name != "orders" || decoded.ReadyStatus != "Ready" {
		t.Errorf("unexpected decoded JSON: %+v", decoded)
	}
}

func TestAgentsGetNamespaceOverride(t *testing.T) {
	isolateHome(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/namespaces" {
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/route-status") {
			_, _ = w.Write([]byte(`{"hasRoute":true}`))
			return
		}
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"metadata":{"name":"orders","namespace":"team2"},"spec":{},"status":{},"workloadType":"deployment","readyStatus":"Ready"}`))
	}))
	t.Cleanup(srv.Close)

	// Current context namespace is team1.
	setupAgentGetContext(t, srv)

	// --namespace team2 must override the context, hitting /agents/team2/orders.
	if _, err := execute(t, "agents", "--namespace", "team2", "get", "orders"); err != nil {
		t.Fatalf("agents get: %v", err)
	}
	if gotPath != "/api/v1/agents/team2/orders" {
		t.Errorf("requested path = %q, want /api/v1/agents/team2/orders", gotPath)
	}
}

func TestAgentsGetRequiresNamespace(t *testing.T) {
	isolateHome(t)
	// Context has no namespace set.
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://x/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	if _, err := execute(t, "agents", "get", "orders"); err == nil {
		t.Error("agents get should error when the current context has no namespace")
	}
}

func TestAgentsGetNamespaceFlagSuppliesWhenContextHasNone(t *testing.T) {
	isolateHome(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/route-status") {
			_, _ = w.Write([]byte(`{"hasRoute":true}`))
			return
		}
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"metadata":{"name":"orders","namespace":"team9"},"spec":{},"status":{},"workloadType":"deployment","readyStatus":"Ready"}`))
	}))
	t.Cleanup(srv.Close)

	// Context has NO namespace; the --namespace flag supplies it.
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	if _, err := execute(t, "agents", "--namespace", "team9", "get", "orders"); err != nil {
		t.Fatalf("agents get: %v", err)
	}
	if gotPath != "/api/v1/agents/team9/orders" {
		t.Errorf("requested path = %q, want /api/v1/agents/team9/orders", gotPath)
	}
}

func TestAgentsGetMinimalAgent(t *testing.T) {
	isolateHome(t)
	// No service, no source, no conditions — the renderer must still work.
	body := `{
		"metadata": {"name": "orders", "namespace": "team1", "labels": {}, "annotations": {}},
		"spec": {},
		"status": {},
		"workloadType": "deployment",
		"readyStatus": "Not Ready"
	}`
	srv := newAgentGetServer(t, body, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "get", "orders")
	if err != nil {
		t.Fatalf("agents get: %v", err)
	}
	for _, want := range []string{
		"Status: Not Ready",
		"No description available",
		"Created:", "N/A",
		"No status conditions available",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("minimal output missing %q:\n%s", want, out)
		}
	}
	// No Service fields when there is no service. The Endpoint section itself
	// may still appear, carrying the external route, which is a property of the
	// agent rather than of its Service — but nothing about a Service that is
	// not there.
	for _, absent := range []string{"Service:", "Cluster IP:", "Ports:"} {
		if strings.Contains(out, absent) {
			t.Errorf("%q should be omitted when there is no service:\n%s", absent, out)
		}
	}
	if strings.Contains(out, "Source") {
		t.Errorf("Source section should be omitted when no git source:\n%s", out)
	}
}

// newAgentGetRouteServer serves the detail path plus a route-status endpoint
// whose status code and body the caller controls, so the tri-state rendering can
// be exercised.
func newAgentGetRouteServer(t *testing.T, routeStatus int, routeBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case strings.HasSuffix(r.URL.Path, "/route-status"):
			if routeStatus != 0 {
				w.WriteHeader(routeStatus)
			}
			_, _ = w.Write([]byte(routeBody))
		case r.URL.Path == "/api/v1/agents/team1/orders":
			_, _ = w.Write([]byte(agentDetailBody))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAgentsGetReportsRoute verifies a route is reported in the Endpoint
// section.
func TestAgentsGetReportsRoute(t *testing.T) {
	isolateHome(t)
	srv := newAgentGetRouteServer(t, 0, `{"hasRoute":true}`)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "get", "orders")
	if err != nil {
		t.Fatalf("agents get: %v", err)
	}
	var routeLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "External Route:") {
			routeLine = strings.TrimSpace(line)
		}
	}
	if routeLine == "" {
		t.Fatalf("output does not report the route:\n%s", out)
	}
	if !strings.HasSuffix(routeLine, "Yes") {
		t.Errorf("route line = %q, want it to end in Yes", routeLine)
	}
}

// TestAgentsGetReportsNoRoute verifies an agent without a route says so, rather
// than omitting the line — false is an answer.
func TestAgentsGetReportsNoRoute(t *testing.T) {
	isolateHome(t)
	srv := newAgentGetRouteServer(t, 0, `{"hasRoute":false}`)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "get", "orders")
	if err != nil {
		t.Fatalf("agents get: %v", err)
	}
	// The line must be present and say No: hasRoute=false is a real answer, so
	// omitting it would lose information the server did supply.
	var routeLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "External Route:") {
			routeLine = strings.TrimSpace(line)
		}
	}
	if routeLine == "" {
		t.Fatalf("output does not report the route:\n%s", out)
	}
	if !strings.HasSuffix(routeLine, "No") {
		t.Errorf("route line = %q, want it to end in No", routeLine)
	}
}

// TestAgentsGetSurvivesRouteStatusFailure verifies a failing route-status call
// does not fail the command: the agent was fetched successfully, and one
// unavailable line is not worth discarding the whole report over.
//
// This also covers a server predating the endpoint, which answers 404.
func TestAgentsGetSurvivesRouteStatusFailure(t *testing.T) {
	isolateHome(t)
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		srv := newAgentGetRouteServer(t, status, `{"detail":"nope"}`)
		setupAgentGetContext(t, srv)

		out, err := execute(t, "agents", "get", "orders")
		if err != nil {
			t.Fatalf("agents get with route-status %d should still succeed: %v", status, err)
		}
		// The agent's own details are still reported in full.
		if !strings.Contains(out, "Agent Information") || !strings.Contains(out, "orders") {
			t.Errorf("route-status %d lost the agent detail:\n%s", status, out)
		}
		// And the route is not asserted either way: an unknown status gets no
		// line, rather than a "No" that would claim the agent has no route when
		// the question was never answered.
		if strings.Contains(out, "External Route") {
			t.Errorf("route-status %d should produce no route line, got:\n%s", status, out)
		}
	}
}

// TestAgentsGetJSONSkipsRouteStatus verifies --json prints the server's agent
// payload unchanged, without the route grafted into it: the flag is documented
// as the raw response, and a synthesized field would make it a different
// document from what the server sent.
func TestAgentsGetJSONSkipsRouteStatus(t *testing.T) {
	isolateHome(t)
	var askedRoute bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case strings.HasSuffix(r.URL.Path, "/route-status"):
			askedRoute = true
			_, _ = w.Write([]byte(`{"hasRoute":true}`))
		default:
			_, _ = w.Write([]byte(agentDetailBody))
		}
	}))
	t.Cleanup(srv.Close)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "get", "orders", "--json")
	if err != nil {
		t.Fatalf("agents get --json: %v", err)
	}
	if askedRoute {
		t.Error("--json should not request route-status; it prints the raw agent response")
	}
	if strings.Contains(out, "hasRoute") {
		t.Errorf("--json output should not carry a synthesized route field:\n%s", out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not valid JSON: %v", err)
	}
}

// TestAgentsGetRouteWithoutService verifies the route is still reported for an
// agent that has no Service. The route is a property of the agent, not of its
// Service, so it must not vanish just because there is nothing to print beside.
func TestAgentsGetRouteWithoutService(t *testing.T) {
	isolateHome(t)
	body := `{
		"metadata": {"name": "orders", "namespace": "team1", "labels": {}, "annotations": {}},
		"spec": {}, "status": {}, "workloadType": "deployment", "readyStatus": "Ready"
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case strings.HasSuffix(r.URL.Path, "/route-status"):
			_, _ = w.Write([]byte(`{"hasRoute":true}`))
		default:
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "get", "orders")
	if err != nil {
		t.Fatalf("agents get: %v", err)
	}
	if !strings.Contains(out, "Endpoint") {
		t.Errorf("Endpoint section should carry the route even with no Service:\n%s", out)
	}
	var routeLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "External Route:") {
			routeLine = strings.TrimSpace(line)
		}
	}
	if !strings.HasSuffix(routeLine, "Yes") {
		t.Errorf("route line = %q, want it to end in Yes", routeLine)
	}
	// Still no Service fields invented for an agent that has none.
	if strings.Contains(out, "Service:") || strings.Contains(out, "Cluster IP:") {
		t.Errorf("no Service fields should appear:\n%s", out)
	}
}
