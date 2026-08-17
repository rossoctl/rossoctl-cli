package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestAgentsImportFromImagePersistentStorage(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "--deployment-type", "statefulset", "from-image",
		"--name", "orders", "--containerImage", "img", "--storage-size", "5Gi"); err != nil {
		t.Fatalf("import: %v", err)
	}
	storage, ok := body["persistentStorage"].(map[string]any)
	if !ok {
		t.Fatalf("persistentStorage = %#v, want object", body["persistentStorage"])
	}
	if storage["enabled"] != true || storage["size"] != "5Gi" {
		t.Errorf("persistentStorage = %#v, want enabled 5Gi", storage)
	}
}

func TestAgentsImportFromImageRejectsStorageForDeployment(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	_, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img", "--storage-size", "5Gi")
	if err == nil || !strings.Contains(err.Error(), "statefulset or sandbox") {
		t.Fatalf("error = %v, want workload compatibility error", err)
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

	// One --envVar alongside the document, naming a variable the document does
	// not: all three arrive, and the document's two come first, which is what
	// makes the merge order observable.
	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--envVarsURL", srv.URL+"/env",
		"--envVar", "EXTRA=fromflag"); err != nil {
		t.Fatalf("import: %v", err)
	}

	envVars, ok := body["envVars"].([]any)
	if !ok || len(envVars) != 3 {
		t.Fatalf("envVars = %+v, want 3 entries", body["envVars"])
	}
	first := envVars[0].(map[string]any)
	if first["name"] != "FOO" || first["value"] != "bar" {
		t.Errorf("envVars[0] = %+v, want {FOO bar}", first)
	}
	if got := renderEnvVars(t, body); got != "FOO=bar BAZ=qux EXTRA=fromflag" {
		t.Errorf("envVars = %q, want the document's pairs before the flag's", got)
	}
}

// renderEnvVars flattens the envVars array of a captured request body into
// "NAME=value NAME=value", so a test can assert on contents *and* order in one
// comparison. Fails the test if the field is missing or malformed.
func renderEnvVars(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, ok := body["envVars"]
	if !ok {
		t.Fatalf("no envVars in the request body: %+v", body)
	}
	entries, ok := raw.([]any)
	if !ok {
		t.Fatalf("envVars = %+v, want an array", raw)
	}
	var parts []string
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("envVars entry = %+v, want an object", e)
		}
		parts = append(parts, fmt.Sprintf("%v=%v", m["name"], m["value"]))
	}
	return strings.Join(parts, " ")
}

// TestAgentsImportFromImageEnvVarFlagOnly verifies --envVar works with no
// --envVarsURL, and that repeating it accumulates.
//
// Two entries is the assertion that matters: a StringVar binding would keep only
// the last, and a RunE that parsed flags only when an env URL was given would
// send none.
func TestAgentsImportFromImageEnvVarFlagOnly(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--envVar", "A=1", "--envVar", "B=2"); err != nil {
		t.Fatalf("import: %v", err)
	}

	if got := renderEnvVars(t, body); got != "A=1 B=2" {
		t.Errorf("envVars = %q, want both flags in the order given", got)
	}
}

// TestAgentsImportFromImageEnvVarOverridesURL verifies --envVar beats
// --envVarsURL for the same name, and that the result does not depend on where
// the flags sit on the command line.
//
// The second invocation is the point. pflag visits flags in lexical name order,
// so the CLI cannot see which came first; the merge order is fixed instead. Only
// running both orders proves that fixed order is actually being applied rather
// than the command line happening to agree with it.
//
// Asserting the full rendering also pins that BAZ keeps its slot: resolving the
// duplicate by removing and re-appending FOO would yield "BAZ=qux FOO=fromflag".
func TestAgentsImportFromImageEnvVarOverridesURL(t *testing.T) {
	isolateHome(t)
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/env":
			_, _ = w.Write([]byte("FOO=fromdoc\nBAZ=qux\n"))
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

	const want = "FOO=fromflag BAZ=qux"

	for _, order := range []struct {
		name string
		args []string
	}{
		{"url first", []string{"--envVarsURL", srv.URL + "/env", "--envVar", "FOO=fromflag"}},
		{"flag first", []string{"--envVar", "FOO=fromflag", "--envVarsURL", srv.URL + "/env"}},
	} {
		t.Run(order.name, func(t *testing.T) {
			body = nil
			args := append([]string{"agents", "import", "from-image",
				"--name", "orders", "--containerImage", "img"}, order.args...)
			if _, err := execute(t, args...); err != nil {
				t.Fatalf("import: %v", err)
			}
			if got := renderEnvVars(t, body); got != want {
				t.Errorf("envVars = %q, want %q regardless of flag position", got, want)
			}
		})
	}
}

// TestAgentsImportFromImageEnvVarLiteralComma verifies a value containing commas
// or JSON punctuation reaches the server intact.
//
// This is what the StringArrayVar choice buys. With StringSliceVar, "TAGS=a,b,c"
// would split into TAGS=a plus bare "b" and "c" (rejected as not key=value, in an
// error naming text the user never typed), and the JSON value would be refused by
// the flag layer with `bare " in non-quoted-field`. Anyone "normalizing" the flag
// to match the rest of the package fails here.
func TestAgentsImportFromImageEnvVarLiteralComma(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--envVar", "TAGS=a,b,c",
		"--envVar", `JSON={"k":"v"}`,
		"--envVar", "EMPTY="); err != nil {
		t.Fatalf("import: %v", err)
	}

	if got := renderEnvVars(t, body); got != `TAGS=a,b,c JSON={"k":"v"} EMPTY=` {
		t.Errorf("envVars = %q; values must arrive literally, unsplit", got)
	}
}

// TestAgentsImportFromImageEnvVarInvalid verifies a malformed --envVar fails the
// command and sends nothing.
//
// The no-request assertion is the substance: parsing after the client call would
// still return an error while having already created the agent.
func TestAgentsImportFromImageEnvVarInvalid(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	_, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img", "--envVar", "FOO")
	if err == nil {
		t.Fatal("expected an error for a --envVar without '='")
	}
	if !strings.Contains(err.Error(), "FOO") {
		t.Errorf("error should quote what the user typed: %v", err)
	}
	if strings.Contains(err.Error(), "line") {
		t.Errorf("a --envVar error must not name a document line: %v", err)
	}
	if body != nil {
		t.Errorf("no agent should have been created, but the server received %+v", body)
	}
}

// TestAgentsImportEnvVarIsRepeatableAcrossRuns verifies flag state does not leak
// between two runs in one process.
//
// A live hazard rather than a hypothetical: resetFlags restores slice defaults
// with pflag's Replace, which leaves pflag's private "changed" bit set, so the
// first Set of a later run appends instead of replacing. That is harmless only
// because the default is nil. Give --envVar a non-nil default and this fails.
func TestAgentsImportEnvVarIsRepeatableAcrossRuns(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img", "--envVar", "A=1"); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if got := renderEnvVars(t, body); got != "A=1" {
		t.Fatalf("first run envVars = %q, want A=1", got)
	}

	body = nil
	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img", "--envVar", "B=2"); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if got := renderEnvVars(t, body); got != "B=2" {
		t.Errorf("second run envVars = %q, want only B=2; the first run's value leaked", got)
	}
}

// TestAgentsImportEnvVarFlagSurface verifies both subcommands document --envVar
// and that it is registered as a string array.
//
// The type assertion catches a switch to StringSliceVar directly, without
// depending on a value that happens to contain a comma — the deterministic
// counterpart to TestAgentsImportFromImageEnvVarLiteralComma.
func TestAgentsImportEnvVarFlagSurface(t *testing.T) {
	isolateHome(t)
	for _, sub := range []string{"from-image", "from-source"} {
		out, err := execute(t, "agents", "import", sub, "--help")
		if err != nil {
			t.Errorf("%s --help: %v", sub, err)
			continue
		}
		if !strings.Contains(out, "--envVar ") {
			t.Errorf("%s --help does not document --envVar:\n%s", sub, out)
		}

		cmd, _, err := rootCmd.Find([]string{"agents", "import", sub})
		if err != nil {
			t.Fatalf("could not find %s: %v", sub, err)
		}
		f := cmd.Flags().Lookup("envVar")
		if f == nil {
			t.Fatalf("%s has no --envVar flag", sub)
		}
		if f.Value.Type() != "stringArray" {
			t.Errorf("%s --envVar is a %s; it must be a stringArray, or values are CSV-split",
				sub, f.Value.Type())
		}
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

// TestAgentsImportCreateHTTPRouteDefault verifies the flag defaults to false and
// that false is sent explicitly rather than omitted.
//
// The presence assertion is the point: the field has no omitempty, so a false
// value must still appear on the wire. Were it dropped, the server would apply
// its own default instead of the value the caller asked for.
func TestAgentsImportCreateHTTPRouteDefault(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img"); err != nil {
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

// TestAgentsImportCreateHTTPRoute verifies the flag reaches the request body.
func TestAgentsImportCreateHTTPRoute(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "--createHttpRoute", "from-image",
		"--name", "orders", "--containerImage", "img"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if body["createHttpRoute"] != true {
		t.Errorf("createHttpRoute = %v, want true", body["createHttpRoute"])
	}
}

// TestAgentsImportCreateHTTPRouteExplicitFalse verifies an explicit =false is
// sent as false, which is the case omitempty would have silently dropped.
func TestAgentsImportCreateHTTPRouteExplicitFalse(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "--createHttpRoute=false", "from-image",
		"--name", "orders", "--containerImage", "img"); err != nil {
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

// TestAgentsImportAdditionalParameterJSON verifies an inline dict reaches the
// request body alongside the fields the flags set.
func TestAgentsImportAdditionalParameterJSON(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--additionalParameterJSON", `{"serviceAccount":"orders-sa","replicas":3}`); err != nil {
		t.Fatalf("import: %v", err)
	}

	if body["serviceAccount"] != "orders-sa" {
		t.Errorf("serviceAccount = %v, want orders-sa", body["serviceAccount"])
	}
	// JSON numbers decode to float64 on this side of the wire; the value is what
	// matters, not the Go type the test's decoder chose.
	if body["replicas"] != float64(3) {
		t.Errorf("replicas = %#v, want 3", body["replicas"])
	}
	// The flags' own fields must survive the overlay.
	if body["name"] != "orders" || body["containerImage"] != "img" {
		t.Errorf("flag-set fields lost: %+v", body)
	}
}

// TestAgentsImportAdditionalParameterJSONFromFile verifies a value naming a file
// sends that file's contents.
func TestAgentsImportAdditionalParameterJSONFromFile(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	path := filepath.Join(t.TempDir(), "extra.json")
	if err := os.WriteFile(path, []byte(`{"nodeSelector":{"disktype":"ssd"}}`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--additionalParameterJSON", path); err != nil {
		t.Fatalf("import: %v", err)
	}

	selector, ok := body["nodeSelector"].(map[string]any)
	if !ok {
		t.Fatalf("nodeSelector = %#v, want an object", body["nodeSelector"])
	}
	if selector["disktype"] != "ssd" {
		t.Errorf("nodeSelector = %#v, want disktype ssd", selector)
	}
}

// TestAgentsImportAdditionalParameterJSONRepeatedMerges verifies repeating the
// flag merges the dicts, with a later value winning a shared key.
func TestAgentsImportAdditionalParameterJSONRepeatedMerges(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	path := filepath.Join(t.TempDir(), "base.json")
	if err := os.WriteFile(path, []byte(`{"fromFile":true,"shared":"file"}`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--additionalParameterJSON", path,
		"--additionalParameterJSON", `{"shared":"inline","fromFlag":true}`); err != nil {
		t.Fatalf("import: %v", err)
	}

	if body["fromFile"] != true || body["fromFlag"] != true {
		t.Errorf("both dicts should contribute: %+v", body)
	}
	if body["shared"] != "inline" {
		t.Errorf("shared = %v, want inline (the later value wins)", body["shared"])
	}
}

// TestAgentsImportAdditionalParameterJSONOverridesFlags verifies a key naming a
// field the CLI already sets replaces it.
//
// The createHttpRoute case is the one worth pinning: it is a bool with no
// omitempty, so it is always present in the encoded request. Overriding it proves
// the overlay replaces a member that is already there rather than only filling in
// absent ones.
func TestAgentsImportAdditionalParameterJSONOverridesFlags(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--additionalParameterJSON",
		`{"containerImage":"override:1","workloadType":"job","createHttpRoute":true}`); err != nil {
		t.Fatalf("import: %v", err)
	}

	if body["containerImage"] != "override:1" {
		t.Errorf("containerImage = %v, want the additional JSON to win", body["containerImage"])
	}
	if body["workloadType"] != "job" {
		t.Errorf("workloadType = %v, want job", body["workloadType"])
	}
	if body["createHttpRoute"] != true {
		t.Errorf("createHttpRoute = %v, want true from the additional JSON", body["createHttpRoute"])
	}
}

// TestAgentsImportAdditionalParameterJSONOverridesPersistentStorage verifies the
// overlay also beats a field assigned after the request literal is built.
//
// --storage-size is set on the struct in a later statement than
// AdditionalParameters, so a naive implementation that merged at construction
// time would have this one field escape the overlay.
func TestAgentsImportAdditionalParameterJSONOverridesPersistentStorage(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "--deployment-type", "statefulset", "from-image",
		"--name", "orders", "--containerImage", "img", "--storage-size", "5Gi",
		"--additionalParameterJSON", `{"persistentStorage":{"enabled":true,"size":"20Gi"}}`); err != nil {
		t.Fatalf("import: %v", err)
	}

	storage, ok := body["persistentStorage"].(map[string]any)
	if !ok {
		t.Fatalf("persistentStorage = %#v, want an object", body["persistentStorage"])
	}
	if storage["size"] != "20Gi" {
		t.Errorf("persistentStorage size = %v, want 20Gi from the additional JSON", storage["size"])
	}
}

// TestAgentsImportAdditionalParameterJSONInvalid verifies a malformed value fails
// the command and sends nothing.
//
// The no-request assertion is the substance, exactly as for --envVar: parsing
// after the client call would still return an error while having already created
// the agent.
func TestAgentsImportAdditionalParameterJSONInvalid(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	_, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--additionalParameterJSON", `{"a":`)
	if err == nil {
		t.Fatal("expected an error for a truncated dict")
	}
	if !strings.Contains(err.Error(), "additionalParameterJSON") {
		t.Errorf("error should name the flag: %v", err)
	}
	if body != nil {
		t.Errorf("no agent should have been created, but the server received %+v", body)
	}
}

// TestAgentsImportAdditionalParameterJSONAbsent verifies the request body is
// unchanged when the flag is not used.
//
// The unknown-key check is the point: an implementation that always merged
// through a map could introduce a stray key (an empty overlay object, say), and a
// server rejecting unknown fields would start failing imports that never asked
// for this feature.
func TestAgentsImportAdditionalParameterJSONAbsent(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img"); err != nil {
		t.Fatalf("import: %v", err)
	}

	known := map[string]bool{
		"name": true, "namespace": true, "deploymentMethod": true, "workloadType": true,
		"envVars": true, "persistentStorage": true, "containerImage": true,
		"imagePullSecret": true, "gitUrl": true, "gitPath": true, "gitBranch": true,
		"createHttpRoute": true,
	}
	for k := range body {
		if !known[k] {
			t.Errorf("unexpected key %q in a request with no --additionalParameterJSON: %+v", k, body)
		}
	}
}

// TestAgentsImportAdditionalParameterJSONFlagSurface verifies both subcommands
// document the flag and that it is a string array.
//
// The type assertion matters here more than for --envVar: a StringSlice would
// split an inline dict on its commas, so '{"a":1,"b":2}' would arrive as the two
// fragments '{"a":1' and 'b":2}', neither of which is JSON.
func TestAgentsImportAdditionalParameterJSONFlagSurface(t *testing.T) {
	isolateHome(t)
	for _, sub := range []string{"from-image", "from-source"} {
		out, err := execute(t, "agents", "import", sub, "--help")
		if err != nil {
			t.Errorf("%s --help: %v", sub, err)
			continue
		}
		if !strings.Contains(out, "--additionalParameterJSON") {
			t.Errorf("%s --help does not document --additionalParameterJSON:\n%s", sub, out)
		}

		cmd, _, err := rootCmd.Find([]string{"agents", "import", sub})
		if err != nil {
			t.Fatalf("could not find %s: %v", sub, err)
		}
		f := cmd.InheritedFlags().Lookup("additionalParameterJSON")
		if f == nil {
			t.Fatalf("%s does not inherit --additionalParameterJSON", sub)
		}
		if f.Value.Type() != "stringArray" {
			t.Errorf("%s --additionalParameterJSON is a %s; it must be a stringArray, or inline dicts are CSV-split",
				sub, f.Value.Type())
		}
	}
}

// TestAgentsImportAdditionalParameterJSONAcrossRuns verifies flag state does not
// leak between two runs in one process, as TestAgentsImportEnvVarIsRepeatableAcrossRuns
// does for --envVar; the same nil-default reasoning applies.
func TestAgentsImportAdditionalParameterJSONAcrossRuns(t *testing.T) {
	isolateHome(t)
	var body map[string]any
	srv := newImportServer(t, &body)
	setupImportContext(t, srv, "team1")

	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--additionalParameterJSON", `{"first":true}`); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if body["first"] != true {
		t.Fatalf("first run did not send its dict: %+v", body)
	}

	body = nil
	if _, err := execute(t, "agents", "import", "from-image",
		"--name", "orders", "--containerImage", "img",
		"--additionalParameterJSON", `{"second":true}`); err != nil {
		t.Fatalf("second import: %v", err)
	}
	if _, leaked := body["first"]; leaked {
		t.Errorf("the first run's dict leaked into the second: %+v", body)
	}
	if body["second"] != true {
		t.Errorf("second run did not send its dict: %+v", body)
	}
}

// TestAgentsImportCreateHTTPRouteIsPersistent verifies the flag is accepted on
// the group and by both subcommands, like --deployment-type.
func TestAgentsImportCreateHTTPRouteIsPersistent(t *testing.T) {
	isolateHome(t)
	for _, sub := range []string{"from-image", "from-source"} {
		if out, err := execute(t, "agents", "import", sub, "--help"); err != nil {
			t.Errorf("%s --help: %v", sub, err)
		} else if !strings.Contains(out, "--createHttpRoute") {
			t.Errorf("%s --help does not document --createHttpRoute:\n%s", sub, out)
		}
	}
}
