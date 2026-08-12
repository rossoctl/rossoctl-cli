package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newToolsServer serves both /tools (returning body) and /namespaces
// (returning a single "default" namespace, used when the command discovers
// namespaces because --namespaces was omitted). The returned pointer captures
// the RawQuery of the most recent /tools request.
func newToolsServer(t *testing.T, body string) (*httptest.Server, *string) {
	t.Helper()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces": ["default"]}`))
		case "/api/v1/tools":
			gotQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &gotQuery
}

const toolsBody = `{
	"items": [
		{
			"name": "weather-mcp",
			"namespace": "team1",
			"description": "Weather tool",
			"status": "Ready",
			"labels": {"protocol": ["mcp"], "framework": null, "type": "tool"},
			"workloadType": "deployment",
			"createdAt": null
		}
	]
}`

func TestToolsListTable(t *testing.T) {
	srv, gotQuery := newToolsServer(t, toolsBody)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "tools", "--namespace", "team1", "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *gotQuery != "namespace=team1" {
		t.Errorf("query = %q, want %q", *gotQuery, "namespace=team1")
	}
	for _, want := range []string{
		"NAME", "NAMESPACE", "STATUS", "WORKLOAD", "PROTOCOL", "DESCRIPTION",
		"weather-mcp", "team1", "Ready", "deployment", "mcp", "Weather tool",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\"items\"") {
		t.Errorf("human output unexpectedly contains raw JSON:\n%s", out)
	}
}

func TestToolsListTableTruncatesDescription(t *testing.T) {
	long := "This tool description exceeds the thirty character limit"
	srv, _ := newToolsServer(t, `{"items":[{"name":"t","namespace":"team1","description":"`+long+
		`","status":"Ready","labels":{"protocol":null,"framework":null,"type":"tool"},"workloadType":null,"createdAt":null}]}`)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "tools", "--namespace", "team1", "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := long[:27] + "..."
	if !strings.Contains(out, want) {
		t.Errorf("table missing truncated description %q:\n%s", want, out)
	}
	if strings.Contains(out, long) {
		t.Errorf("table contains untruncated description:\n%s", out)
	}
}

func TestToolsListJSON(t *testing.T) {
	srv, _ := newToolsServer(t, toolsBody)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "tools", "--namespace", "team1", "list", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if len(decoded.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(decoded.Items))
	}
}

func TestToolsListAllNamespacesDiscovers(t *testing.T) {
	var toolsQueried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces": ["alpha", "beta"]}`))
		case "/api/v1/tools":
			ns := r.URL.Query().Get("namespace")
			toolsQueried = append(toolsQueried, ns)
			_, _ = w.Write([]byte(`{"items":[{"name":"t-` + ns + `","namespace":"` + ns +
				`","description":"d","status":"Ready","labels":{"protocol":null,"framework":null,"type":"tool"},"workloadType":null,"createdAt":null}]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "tools", "list", "--all-namespaces")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(toolsQueried) != 2 || toolsQueried[0] != "alpha" || toolsQueried[1] != "beta" {
		t.Errorf("tools queried for namespaces %v, want [alpha beta]", toolsQueried)
	}
	for _, want := range []string{"t-alpha", "alpha", "t-beta", "beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Single combined table.
	if n := strings.Count(out, "NAMESPACE"); n != 1 {
		t.Errorf("expected one table header, found %d:\n%s", n, out)
	}
}

func TestToolsListDefaultUsesSingleNamespaceNoDiscovery(t *testing.T) {
	// Errors if /namespaces is hit, proving no discovery without --all-namespaces.
	var queriedNS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tools" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		queriedNS = r.URL.Query().Get("namespace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := execute(t, "--server", srv.URL+"/api/v1/", "tools", "--namespace", "team1", "list"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if queriedNS != "team1" {
		t.Errorf("queried namespace = %q, want team1", queriedNS)
	}
}

func TestToolsListAllNamespacesJSONSeparator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces": ["team1", "team2"]}`))
		case "/api/v1/tools":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "tools", "list", "--all-namespaces", "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parts := strings.Split(out, "---\n")
	if len(parts) != 2 {
		t.Fatalf("expected 2 JSON parts separated by ---, got %d:\n%s", len(parts), out)
	}
}

// TestToolsListNoHeaders verifies --no-headers drops the header row and keeps
// every data row.
//
// The header words are checked individually rather than as one line, because
// tabwriter pads columns: asserting the joined string would pass if the header
// were merely reformatted. Each word is also absent from this fixture's data, so
// finding one can only mean the header survived.
func TestToolsListNoHeaders(t *testing.T) {
	srv, _ := newToolsServer(t, toolsBody)

	out, err := execute(t, "--server", srv.URL+"/api/v1/",
		"tools", "--namespace", "team1", "list", "--no-headers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, unwanted := range []string{"NAME", "NAMESPACE", "STATUS", "WORKLOAD", "PROTOCOL", "DESCRIPTION"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("--no-headers output still contains the header %q:\n%s", unwanted, out)
		}
	}
	// The rows themselves must survive; a flag that suppressed everything would
	// pass the checks above.
	for _, want := range []string{"weather-mcp", "team1", "Ready", "deployment", "mcp"} {
		if !strings.Contains(out, want) {
			t.Errorf("--no-headers output missing data %q:\n%s", want, out)
		}
	}
}

// TestToolsListNoHeadersFeedsXargs is the test for the issue that asked for this
// flag: `tools list | awk '{print $1}' | xargs -n1 tools delete` fed the header
// to the delete command, which then reported on a tool named "NAME".
//
// It asserts on the first field of every stdout line, which is what awk would
// take, rather than on the absence of the word "NAME" -- that is the property the
// pipeline actually depends on.
func TestToolsListNoHeadersFeedsXargs(t *testing.T) {
	body := `{"items": [
		{"name": "alpha", "namespace": "team1", "status": "Ready",
		 "labels": {"protocol": ["mcp"], "framework": null, "type": "tool"},
		 "workloadType": "deployment", "description": "a", "createdAt": null},
		{"name": "beta", "namespace": "team1", "status": "Ready",
		 "labels": {"protocol": ["mcp"], "framework": null, "type": "tool"},
		 "workloadType": "deployment", "description": "b", "createdAt": null}
	]}`
	srv, _ := newToolsServer(t, body)

	stdout, _, err := executeSplit(t, "--server", srv.URL+"/api/v1/",
		"tools", "--namespace", "team1", "list", "--no-headers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var names []string
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		names = append(names, strings.Fields(line)[0])
	}
	want := []string{"alpha", "beta"}
	if len(names) != len(want) {
		t.Fatalf("first fields = %v, want exactly %v (one per tool, no header)", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("first field %d = %q, want %q", i, names[i], want[i])
		}
	}
}

// TestToolsListNoHeadersEmptyStdoutIsBlank verifies stdout is empty when there is
// nothing to list, with the notice on stderr instead.
//
// Without this, a pipeline over an empty namespace passes "No" to the next
// command -- the same class of bug as the header row, and the reason the notice
// changes stream with the flag. executeSplit is required here: the shared execute
// helper merges both streams, so it cannot tell the two apart and would pass
// either way.
func TestToolsListNoHeadersEmptyStdoutIsBlank(t *testing.T) {
	srv, _ := newToolsServer(t, `{"items": []}`)

	stdout, stderr, err := executeSplit(t, "--server", srv.URL+"/api/v1/",
		"tools", "--namespace", "team1", "list", "--no-headers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty so a pipeline sees no rows", stdout)
	}
	if !strings.Contains(stderr, "No tools found") {
		t.Errorf("stderr = %q, want the notice reported there", stderr)
	}
}

// TestToolsListEmptyNoticeOnStdoutByDefault is the complement: without the flag
// the notice stays on stdout, where it has always been. Guards against fixing the
// pipeline by moving the line unconditionally, which would hide it from anyone
// redirecting stdout to a file.
func TestToolsListEmptyNoticeOnStdoutByDefault(t *testing.T) {
	srv, _ := newToolsServer(t, `{"items": []}`)

	stdout, _, err := executeSplit(t, "--server", srv.URL+"/api/v1/",
		"tools", "--namespace", "team1", "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, "No tools found") {
		t.Errorf("stdout = %q, want the notice on stdout without --no-headers", stdout)
	}
}

// TestToolsListNoHeadersWithJSONIsUnchanged verifies --no-headers does not touch
// the JSON output, which has no headers to remove. A flag that stripped a line
// from JSON would produce something that no longer parses.
func TestToolsListNoHeadersWithJSONIsUnchanged(t *testing.T) {
	srv, _ := newToolsServer(t, toolsBody)

	withFlag, _, err := executeSplit(t, "--server", srv.URL+"/api/v1/",
		"tools", "--namespace", "team1", "list", "--json", "--no-headers")
	if err != nil {
		t.Fatalf("with --no-headers: %v", err)
	}
	withoutFlag, _, err := executeSplit(t, "--server", srv.URL+"/api/v1/",
		"tools", "--namespace", "team1", "list", "--json")
	if err != nil {
		t.Fatalf("without --no-headers: %v", err)
	}
	if withFlag != withoutFlag {
		t.Errorf("--no-headers changed the JSON output:\nwith:\n%s\nwithout:\n%s", withFlag, withoutFlag)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(withFlag), &parsed); err != nil {
		t.Errorf("JSON output does not parse: %v\n%s", err, withFlag)
	}
}

func TestToolsListEmpty(t *testing.T) {
	srv, _ := newToolsServer(t, `{"items": []}`)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "tools", "--namespace", "team1", "list")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "No tools found") {
		t.Errorf("empty output = %q, want %q", out, "No tools found")
	}
}
