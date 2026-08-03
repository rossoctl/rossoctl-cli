package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentsImportIsGroup(t *testing.T) {
	// Running the group with no subcommand shows help, not the standalone
	// UNIMPLEMENTED placeholder line.
	out, err := execute(t, "agents", "import")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "UNIMPLEMENTED" {
			t.Errorf("`agents import` executed a stub; expected help:\n%s", out)
		}
	}
	for _, sub := range []string{"from-image", "from-source"} {
		if !strings.Contains(out, sub) {
			t.Errorf("`agents import` help missing subcommand %q:\n%s", sub, out)
		}
	}
	// The old `deploy` name must be gone from the command tree.
	if c, _, _ := rootCmd.Find([]string{"agents", "deploy"}); c != nil && c.Name() == "deploy" {
		t.Error("`agents deploy` should no longer exist")
	}
}

func TestAgentsImportFromSourceUnimplemented(t *testing.T) {
	out, err := execute(t, "agents", "import", "from-source")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "UNIMPLEMENTED") {
		t.Errorf("output = %q, want UNIMPLEMENTED", out)
	}
}

// newImportServer serves /namespaces (for context validation) and captures the
// POST /agents body.
func newImportServer(t *testing.T, gotBody *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))
		case r.URL.Path == "/api/v1/agents" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(gotBody)
			_, _ = w.Write([]byte(`{"success":true,"name":"orders","namespace":"team1","message":"Agent created"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setupImportContext(t *testing.T, srv *httptest.Server, namespace string) {
	t.Helper()
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	if _, err := execute(t, "config", "set-context", "--namespace", namespace); err != nil {
		t.Fatalf("set-context: %v", err)
	}
}

func TestAgentsImportFromImagePostsRequest(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	out, err := execute(t, "agents", "import", "from-image",
		"--name", "orders",
		"--containerImage", "ghcr.io/x/y:latest",
		"--imagePullSecret", "regcred",
	)
	if err != nil {
		t.Fatalf("import from-image: %v", err)
	}

	if body["name"] != "orders" || body["namespace"] != "team1" {
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
	if !strings.Contains(out, "Agent created") {
		t.Errorf("output missing server message:\n%s", out)
	}
}

func TestAgentsImportFromImageDeploymentType(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "--deployment-type", "sandbox", "from-image",
		"--name", "orders", "--containerImage", "img"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if body["workloadType"] != "sandbox" {
		t.Errorf("workloadType = %v, want sandbox", body["workloadType"])
	}
}

func TestAgentsImportFromImageNamespaceOverride(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	// agents --namespace overrides the context's team1.
	if _, err := execute(t, "agents", "--namespace", "team2", "import", "from-image",
		"--name", "orders", "--containerImage", "img"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if body["namespace"] != "team2" {
		t.Errorf("namespace = %v, want team2 (override)", body["namespace"])
	}
}

func TestAgentsImportFromImageEnvVars(t *testing.T) {
	isolateHome(t)
	var body map[string]any

	// A server that also serves the env-vars document at /env.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/env":
			_, _ = w.Write([]byte("FOO=bar\n# comment\nBAZ=qux\n"))
		case r.URL.Path == "/api/v1/namespaces":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case r.URL.Path == "/api/v1/agents" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"success":true,"message":"ok"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
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

func TestAgentsImportFromImageRequiresNameAndImage(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image", "--containerImage", "img"); err == nil {
		t.Error("expected error when --name is missing")
	}
	if _, err := execute(t, "agents", "import", "from-image", "--name", "orders"); err == nil {
		t.Error("expected error when --containerImage is missing")
	}
}

func TestAgentsImportDeploymentTypeDefault(t *testing.T) {
	// --deployment-type is a persistent flag on the import group, inherited by
	// the subcommands, defaulting to "deployment".
	cmd, _, err := rootCmd.Find([]string{"agents", "import", "from-image"})
	if err != nil {
		t.Fatalf("could not find command: %v", err)
	}
	// Look the flag up via InheritedFlags rather than Flags: Cobra only merges a
	// parent's persistent flags into a subcommand's own set when that command is
	// executed, so Flags() finds it only if some earlier test happened to run an
	// `agents import` command. InheritedFlags resolves the parent chain directly,
	// which keeps this test independent of ordering.
	f := cmd.InheritedFlags().Lookup("deployment-type")
	if f == nil {
		t.Fatal("from-image does not inherit --deployment-type")
	}
	if f.DefValue != "deployment" {
		t.Errorf("--deployment-type default = %q, want deployment", f.DefValue)
	}
}

func TestAgentsImportFromSourceGitBranchDefault(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"agents", "import", "from-source"})
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
