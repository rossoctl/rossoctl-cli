package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const agentsBody = `{
	"items": [
		{
			"name": "orders-agent",
			"namespace": "team1",
			"description": "Handles orders",
			"status": "Ready",
			"labels": {"protocol": ["a2a"], "framework": "LangGraph", "type": "agent"},
			"workloadType": "deployment",
			"createdAt": "2026-01-02T03:04:05Z"
		},
		{
			"name": "weather",
			"namespace": "team1",
			"description": "Weather agent",
			"status": "Not Ready",
			"labels": {"protocol": null, "framework": null, "type": "agent"},
			"workloadType": null,
			"createdAt": null
		}
	]
}`

// newAgentsServer serves both /agents (returning body) and /namespaces
// (returning a single "default" namespace, used when the command discovers
// namespaces because --namespaces was omitted). The returned pointer captures
// the RawQuery of the most recent /agents request.
func newAgentsServer(t *testing.T, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces": ["default"]}`))
		case "/api/v1/agents":
			gotQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gotQuery
}

func TestAgentsListTable(t *testing.T) {
	srv, _ := newAgentsServer(t, agentsBody)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "--namespace", "default", "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{
		"NAME", "NAMESPACE", "STATUS", "WORKLOAD", "PROTOCOL", "DESCRIPTION",
		"orders-agent", "team1", "Ready", "deployment", "a2a", "Handles orders",
		"weather", "Not Ready",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}

	// The nil workloadType/protocol should render as "-", not "<nil>".
	if strings.Contains(out, "<nil>") {
		t.Errorf("table rendered a nil pointer:\n%s", out)
	}
	// Human output must not be raw JSON.
	if strings.Contains(out, "\"items\"") {
		t.Errorf("human output unexpectedly contains raw JSON:\n%s", out)
	}
}

func TestAgentsListTableTruncatesDescription(t *testing.T) {
	long := "This description is definitely longer than thirty characters"
	srv, _ := newAgentsServer(t, `{"items":[{"name":"a","namespace":"team1","description":"`+long+
		`","status":"Ready","labels":{"protocol":null,"framework":null,"type":"agent"},"workloadType":null,"createdAt":null}]}`)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "--namespace", "team1", "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := long[:27] + "..."
	if !strings.Contains(out, want) {
		t.Errorf("table missing truncated description %q:\n%s", want, out)
	}
	// The full description must not appear.
	if strings.Contains(out, long) {
		t.Errorf("table contains untruncated description:\n%s", out)
	}
}

func TestAgentsListJSON(t *testing.T) {
	srv, _ := newAgentsServer(t, agentsBody)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "--namespace", "default", "list", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if len(decoded.Items) != 2 {
		t.Errorf("expected 2 items, got %d", len(decoded.Items))
	}
}

func TestAgentsListVerboseLogsToStderr(t *testing.T) {
	srv, _ := newAgentsServer(t, `{"items": []}`)

	stdout, stderr, err := executeSplit(t,
		"--verbose", "--server", srv.URL+"/api/v1/", "agents", "--namespace", "default", "list", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The request line must be logged to stderr, including method and URL.
	if !strings.Contains(stderr, "GET "+srv.URL+"/api/v1/agents") {
		t.Errorf("stderr missing request log:\n%s", stderr)
	}
	// And the response status.
	if !strings.Contains(stderr, "200 OK") {
		t.Errorf("stderr missing response log:\n%s", stderr)
	}
	// stdout must stay clean JSON — no log noise mixed in.
	if strings.Contains(stdout, "GET ") {
		t.Errorf("stdout contains verbose log output:\n%s", stdout)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not clean JSON with --verbose: %v\n%s", err, stdout)
	}
}

func TestAgentsListNoVerboseNoLog(t *testing.T) {
	srv, _ := newAgentsServer(t, `{"items": []}`)

	_, stderr, err := executeSplit(t, "--server", srv.URL+"/api/v1/", "agents", "--namespace", "default", "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(stderr, "GET ") {
		t.Errorf("expected no request logging without --verbose, got:\n%s", stderr)
	}
}

func TestAgentsListSingleNamespaceFromFlag(t *testing.T) {
	srv, gotQuery := newAgentsServer(t, `{"items": []}`)

	// No --all-namespaces: the agents --namespace flag selects the one namespace.
	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "--namespace", "team1", "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *gotQuery != "namespace=team1" {
		t.Errorf("query = %q, want %q", *gotQuery, "namespace=team1")
	}
	if !strings.Contains(out, "No agents found") {
		t.Errorf("empty list output = %q, want %q", out, "No agents found")
	}
}

// TestAgentsListNoHeaders mirrors TestToolsListNoHeaders. The two print
// functions are separate copies of the same code, so each needs its own guard --
// a change applied to one and not the other is exactly what this catches.
func TestAgentsListNoHeaders(t *testing.T) {
	srv, _ := newAgentsServer(t, agentsBody)

	out, err := execute(t, "--server", srv.URL+"/api/v1/",
		"agents", "--namespace", "team1", "list", "--no-headers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, unwanted := range []string{"NAME", "NAMESPACE", "STATUS", "WORKLOAD", "PROTOCOL", "DESCRIPTION"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("--no-headers output still contains the header %q:\n%s", unwanted, out)
		}
	}
	for _, want := range []string{"orders-agent", "weather", "team1", "Ready", "a2a"} {
		if !strings.Contains(out, want) {
			t.Errorf("--no-headers output missing data %q:\n%s", want, out)
		}
	}

	// The first field of each line is what `awk '{print $1}'` would take.
	var names []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		names = append(names, strings.Fields(line)[0])
	}
	if len(names) != 2 || names[0] != "orders-agent" || names[1] != "weather" {
		t.Errorf("first fields = %v, want [orders-agent weather] with no header row", names)
	}
}

// TestAgentsListNoHeadersEmptyStdoutIsBlank mirrors the tools case: stdout must
// be empty when there is nothing to list, so a pipeline does not receive the word
// "No" as an argument.
func TestAgentsListNoHeadersEmptyStdoutIsBlank(t *testing.T) {
	srv, _ := newAgentsServer(t, `{"items": []}`)

	stdout, stderr, err := executeSplit(t, "--server", srv.URL+"/api/v1/",
		"agents", "--namespace", "team1", "list", "--no-headers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty so a pipeline sees no rows", stdout)
	}
	if !strings.Contains(stderr, "No agents found") {
		t.Errorf("stderr = %q, want the notice reported there", stderr)
	}
}

// TestAgentsListEmptyNoticeOnStdoutByDefault pins the unflagged behavior, so the
// pipeline fix cannot be made by moving the notice unconditionally.
func TestAgentsListEmptyNoticeOnStdoutByDefault(t *testing.T) {
	srv, _ := newAgentsServer(t, `{"items": []}`)

	stdout, _, err := executeSplit(t, "--server", srv.URL+"/api/v1/",
		"agents", "--namespace", "team1", "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, "No agents found") {
		t.Errorf("stdout = %q, want the notice on stdout without --no-headers", stdout)
	}
}

func TestAgentsListRequiresNamespaceWithoutAllFlag(t *testing.T) {
	// No --all-namespaces and no --namespace/context namespace -> error.
	isolateHome(t)
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://x/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	if _, err := execute(t, "agents", "list"); err == nil {
		t.Error("agents list without --all-namespaces or a namespace should error")
	}
}

// newPerNamespaceAgentsServer returns a server that responds based on the
// requested namespace, recording every namespace it was queried for in order.
func newPerNamespaceAgentsServer(t *testing.T, byNamespace map[string]string) (*httptest.Server, *[]string) {
	t.Helper()
	var queried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		ns := r.URL.Query().Get("namespace")
		queried = append(queried, ns)
		body, ok := byNamespace[ns]
		if !ok {
			body = `{"items": []}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &queried
}

func TestAgentsListAllNamespacesDiscovers(t *testing.T) {
	var agentsQueried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			// enabled-only discovery => no enabled_only=false query param.
			if r.URL.RawQuery != "" {
				t.Errorf("namespaces query = %q, want empty (enabled-only)", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"namespaces": ["alpha", "beta"]}`))
		case "/api/v1/agents":
			ns := r.URL.Query().Get("namespace")
			agentsQueried = append(agentsQueried, ns)
			_, _ = w.Write([]byte(`{"items":[{"name":"a-` + ns + `","namespace":"` + ns +
				`","description":"d","status":"Ready","labels":{"protocol":null,"framework":null,"type":"agent"},"workloadType":null,"createdAt":null}]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	// --all-namespaces: discover [alpha, beta] and query each.
	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "list", "--all-namespaces")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(agentsQueried) != 2 || agentsQueried[0] != "alpha" || agentsQueried[1] != "beta" {
		t.Errorf("agents queried for namespaces %v, want [alpha beta]", agentsQueried)
	}
	for _, want := range []string{"a-alpha", "alpha", "a-beta", "beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestAgentsListAllNamespacesNoNamespaces(t *testing.T) {
	agentsCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces": []}`))
		case "/api/v1/agents":
			agentsCalled = true
			_, _ = w.Write([]byte(`{"items": []}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "list", "--all-namespaces")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No discovered namespaces => no /agents requests at all.
	if agentsCalled {
		t.Error("expected no /agents request when no namespaces are discovered")
	}
	if !strings.Contains(out, "No agents found") {
		t.Errorf("output = %q, want %q", out, "No agents found")
	}
}

func TestAgentsListDefaultUsesSingleNamespaceNoDiscovery(t *testing.T) {
	// The per-namespace server errors if /namespaces is hit, so this proves
	// that without --all-namespaces the command does NOT discover.
	srv, queried := newPerNamespaceAgentsServer(t, map[string]string{
		"team1": `{"items":[{"name":"orders","namespace":"team1","description":"d","status":"Ready","labels":{"protocol":null,"framework":null,"type":"agent"},"workloadType":null,"createdAt":null}]}`,
	})

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "--namespace", "team1", "list")
	if err != nil {
		t.Fatalf("agents list: %v", err)
	}
	if len(*queried) != 1 || (*queried)[0] != "team1" {
		t.Errorf("queried namespaces = %v, want [team1]", *queried)
	}
	if !strings.Contains(out, "orders") {
		t.Errorf("output missing agent:\n%s", out)
	}
}

func TestAgentsListAllNamespacesCombinedTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces": ["team1", "team2"]}`))
		case "/api/v1/agents":
			switch r.URL.Query().Get("namespace") {
			case "team1":
				_, _ = w.Write([]byte(`{"items":[{"name":"orders","namespace":"team1","description":"d1","status":"Ready","labels":{"protocol":["a2a"],"framework":null,"type":"agent"},"workloadType":"deployment","createdAt":null}]}`))
			case "team2":
				_, _ = w.Write([]byte(`{"items":[{"name":"weather","namespace":"team2","description":"d2","status":"Not Ready","labels":{"protocol":null,"framework":null,"type":"agent"},"workloadType":null,"createdAt":null}]}`))
			default:
				_, _ = w.Write([]byte(`{"items":[]}`))
			}
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "list", "--all-namespaces")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both namespaces' agents appear in the single combined table.
	for _, want := range []string{"orders", "team1", "weather", "team2"} {
		if !strings.Contains(out, want) {
			t.Errorf("combined table missing %q:\n%s", want, out)
		}
	}
	// One header only (single table).
	if n := strings.Count(out, "NAMESPACE"); n != 1 {
		t.Errorf("expected exactly one table header, found %d:\n%s", n, out)
	}
}

func TestAgentsListAllNamespacesJSONSeparator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces": ["team1", "team2"]}`))
		case "/api/v1/agents":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "list", "--all-namespaces", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// One JSON document per discovered namespace, separated by "---".
	parts := strings.Split(out, "---\n")
	if len(parts) != 2 {
		t.Fatalf("expected 2 JSON parts separated by ---, got %d:\n%s", len(parts), out)
	}
	for i, part := range parts {
		var decoded struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(part), &decoded); err != nil {
			t.Errorf("part %d is not valid JSON: %v\n%s", i, err, part)
		}
	}
}

func TestAgentsListSingleJSONHasNoSeparator(t *testing.T) {
	srv, _ := newAgentsServer(t, `{"items": []}`)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "--namespace", "default", "list", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "---") {
		t.Errorf("single-namespace JSON should have no separator:\n%s", out)
	}
}
