package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToolsImportIsGroup(t *testing.T) {
	out, err := execute(t, "tools", "import")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "UNIMPLEMENTED" {
			t.Errorf("`tools import` executed a stub; expected help:\n%s", out)
		}
	}
	for _, sub := range []string{"from-image", "from-source"} {
		if !strings.Contains(out, sub) {
			t.Errorf("`tools import` help missing subcommand %q:\n%s", sub, out)
		}
	}
}

func TestToolsImportFromSourceUnimplemented(t *testing.T) {
	out, err := execute(t, "tools", "import", "from-source")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "UNIMPLEMENTED") {
		t.Errorf("output = %q, want UNIMPLEMENTED", out)
	}
}

// newToolsImportServer serves /namespaces (for context validation) and
// captures the POST /tools body.
func newToolsImportServer(t *testing.T, gotBody *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))
		case r.URL.Path == "/api/v1/tools" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(gotBody)
			_, _ = w.Write([]byte(`{"success":true,"name":"weather-mcp","namespace":"team1","message":"Tool created"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupToolsImportContext(t *testing.T, srv *httptest.Server, namespace string) {
	t.Helper()
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	if _, err := execute(t, "config", "set-context", "--namespace", namespace); err != nil {
		t.Fatalf("set-context: %v", err)
	}
}

func TestToolsImportFromImagePostsRequest(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newToolsImportServer(t, &body)
	setupToolsImportContext(t, srv, "team1")

	out, err := execute(t, "tools", "import", "from-image",
		"--name", "weather-mcp",
		"--containerImage", "ghcr.io/x/y:latest",
		"--imagePullSecret", "regcred",
	)
	if err != nil {
		t.Fatalf("import from-image: %v", err)
	}

	if body["name"] != "weather-mcp" || body["namespace"] != "team1" {
		t.Errorf("name/namespace wrong: %+v", body)
	}
	if body["deploymentMethod"] != "image" {
		t.Errorf("deploymentMethod = %v, want image", body["deploymentMethod"])
	}
	if body["workloadType"] != "deployment" {
		t.Errorf("workloadType = %v, want deployment (default)", body["workloadType"])
	}
	if body["containerImage"] != "ghcr.io/x/y:latest" || body["imagePullSecret"] != "regcred" {
		t.Errorf("image fields wrong: %+v", body)
	}
	// The default --ports must send a single http:9090:9090:TCP service port.
	sp := servicePortsFromBody(t, body)
	if len(sp) != 1 {
		t.Fatalf("servicePorts = %+v, want 1 default entry", sp)
	}
	if sp[0]["name"] != "http" || sp[0]["port"] != float64(9090) ||
		sp[0]["targetPort"] != float64(9090) || sp[0]["protocol"] != "TCP" {
		t.Errorf("default servicePort = %+v, want http/9090/9090/TCP", sp[0])
	}
	if !strings.Contains(out, "Tool created") {
		t.Errorf("output missing server message:\n%s", out)
	}
}

func servicePortsFromBody(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["servicePorts"].([]any)
	if !ok {
		t.Fatalf("servicePorts missing or wrong type: %+v", body["servicePorts"])
	}
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}

func TestToolsImportFromImagePortsFlag(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newToolsImportServer(t, &body)
	setupToolsImportContext(t, srv, "team1")

	// Custom ports: a full spec and a bare-port shorthand.
	if _, err := execute(t, "tools", "import", "from-image",
		"--name", "weather-mcp", "--containerImage", "img",
		"--ports", "grpc:9000:9001:TCP,8080"); err != nil {
		t.Fatalf("import: %v", err)
	}

	sp := servicePortsFromBody(t, body)
	if len(sp) != 2 {
		t.Fatalf("servicePorts = %+v, want 2 entries", sp)
	}
	if sp[0]["name"] != "grpc" || sp[0]["port"] != float64(9000) ||
		sp[0]["targetPort"] != float64(9001) || sp[0]["protocol"] != "TCP" {
		t.Errorf("port[0] = %+v, want grpc/9000/9001/TCP", sp[0])
	}
	// Bare "8080" -> http:8080:8080:TCP.
	if sp[1]["name"] != "http" || sp[1]["port"] != float64(8080) ||
		sp[1]["targetPort"] != float64(8080) || sp[1]["protocol"] != "TCP" {
		t.Errorf("port[1] = %+v, want http/8080/8080/TCP", sp[1])
	}
}

func TestToolsImportFromImageDeploymentType(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newToolsImportServer(t, &body)
	setupToolsImportContext(t, srv, "team1")

	if _, err := execute(t, "tools", "import", "--deployment-type", "statefulset", "from-image",
		"--name", "weather-mcp", "--containerImage", "img"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if body["workloadType"] != "statefulset" {
		t.Errorf("workloadType = %v, want statefulset", body["workloadType"])
	}
}

func TestToolsImportFromImageNamespaceOverride(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newToolsImportServer(t, &body)
	setupToolsImportContext(t, srv, "team1")

	if _, err := execute(t, "tools", "--namespace", "team2", "import", "from-image",
		"--name", "weather-mcp", "--containerImage", "img"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if body["namespace"] != "team2" {
		t.Errorf("namespace = %v, want team2 (override)", body["namespace"])
	}
}

func TestToolsImportFromImageEnvVars(t *testing.T) {
	isolateHome(t)
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/env":
			_, _ = w.Write([]byte("FOO=bar\n# comment\nBAZ=qux\n"))
		case r.URL.Path == "/api/v1/namespaces":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case r.URL.Path == "/api/v1/tools" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"success":true,"message":"ok"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	setupToolsImportContext(t, srv, "team1")

	if _, err := execute(t, "tools", "import", "from-image",
		"--name", "weather-mcp", "--containerImage", "img",
		"--envVarsURL", srv.URL+"/env"); err != nil {
		t.Fatalf("import: %v", err)
	}

	envVars, ok := body["envVars"].([]any)
	if !ok || len(envVars) != 2 {
		t.Fatalf("envVars = %+v, want 2 entries", body["envVars"])
	}
	first := envVars[0].(map[string]any)
	if first["name"] != "FOO" || first["value"] != "bar" {
		t.Errorf("envVars[0] = %+v, want {FOO bar}", first)
	}
}

func TestToolsImportFromImageRequiresNameAndImage(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newToolsImportServer(t, &body)
	setupToolsImportContext(t, srv, "team1")

	if _, err := execute(t, "tools", "import", "from-image", "--containerImage", "img"); err == nil {
		t.Error("expected error when --name is missing")
	}
	if _, err := execute(t, "tools", "import", "from-image", "--name", "weather-mcp"); err == nil {
		t.Error("expected error when --containerImage is missing")
	}
}

func TestToolsImportDeploymentTypeDefault(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"tools", "import", "from-image"})
	if err != nil {
		t.Fatalf("could not find command: %v", err)
	}
	// InheritedFlags, not Flags: see the note in the agents equivalent — Cobra
	// only merges a parent's persistent flags into Flags() once the command runs,
	// so Flags() would make this test depend on test ordering.
	f := cmd.InheritedFlags().Lookup("deployment-type")
	if f == nil {
		t.Fatal("from-image does not inherit --deployment-type")
	}
	if f.DefValue != "deployment" {
		t.Errorf("--deployment-type default = %q, want deployment", f.DefValue)
	}
}

func TestToolsImportFromSourceGitBranchDefault(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"tools", "import", "from-source"})
	if err != nil {
		t.Fatalf("could not find command: %v", err)
	}
	f := cmd.Flags().Lookup("gitBranch")
	if f == nil {
		t.Fatal("from-source has no --gitBranch flag")
	}
	if f.DefValue != "main" {
		t.Errorf("--gitBranch default = %q, want %q", f.DefValue, "main")
	}
}

// TestToolsImportCreateHTTPRouteDefault verifies the flag defaults to false and
// that false is sent explicitly rather than omitted.
//
// The presence assertion is the point: the field has no omitempty, so a false
// value must still appear on the wire. Were it dropped, the server would apply
// its own default instead of the value the caller asked for.
func TestToolsImportCreateHTTPRouteDefault(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newToolsImportServer(t, &body)
	setupToolsImportContext(t, srv, "team1")

	if _, err := execute(t, "tools", "import", "from-image",
		"--name", "weather-mcp", "--containerImage", "img"); err != nil {
		t.Fatalf("import: %v", err)
	}

	got, ok := body["createHttpRoute"]
	if !ok {
		t.Fatalf("createHttpRoute missing from the request body: %+v", body)
	}
	if got != false {
		t.Errorf("createHttpRoute = %v, want false (default)", got)
	}
}

// TestToolsImportCreateHTTPRoute verifies the flag reaches the request body.
func TestToolsImportCreateHTTPRoute(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newToolsImportServer(t, &body)
	setupToolsImportContext(t, srv, "team1")

	if _, err := execute(t, "tools", "import", "--createHttpRoute", "from-image",
		"--name", "weather-mcp", "--containerImage", "img"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if body["createHttpRoute"] != true {
		t.Errorf("createHttpRoute = %v, want true", body["createHttpRoute"])
	}
}

// TestToolsImportCreateHTTPRouteExplicitFalse verifies an explicit =false is
// sent as false, which is the case omitempty would have silently dropped.
func TestToolsImportCreateHTTPRouteExplicitFalse(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newToolsImportServer(t, &body)
	setupToolsImportContext(t, srv, "team1")

	if _, err := execute(t, "tools", "import", "--createHttpRoute=false", "from-image",
		"--name", "weather-mcp", "--containerImage", "img"); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, ok := body["createHttpRoute"]
	if !ok {
		t.Fatalf("createHttpRoute missing from the request body: %+v", body)
	}
	if got != false {
		t.Errorf("createHttpRoute = %v, want false", got)
	}
}

// TestToolsImportCreateHTTPRouteIsPersistent verifies the flag is accepted on
// the group and by both subcommands, like --deployment-type.
func TestToolsImportCreateHTTPRouteIsPersistent(t *testing.T) {
	isolateHome(t)
	for _, sub := range []string{"from-image", "from-source"} {
		if out, err := execute(t, "tools", "import", sub, "--help"); err != nil {
			t.Errorf("%s --help: %v", sub, err)
		} else if !strings.Contains(out, "--createHttpRoute") {
			t.Errorf("%s --help does not document --createHttpRoute:\n%s", sub, out)
		}
	}
}
