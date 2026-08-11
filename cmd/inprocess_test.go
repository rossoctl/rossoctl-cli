package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/instances"
	"github.com/rossoctl/rossoctl-cli/internal/rossoctlclient"
	"github.com/rossoctl/rossoctl-cli/internal/serve"
)

// deadServerURI is an address nothing listens on, standing in for a
// `rossoctl cortex serve` that was never started. Port 1 is used because it is
// privileged and reserved, so a stray developer process is not going to be bound
// there.
const deadServerURI = "http://127.0.0.1:1/api/v1/"

// a2aInstance and mcpInstance describe one recorded local instance, as
// `authbridge exec` would have left behind.
func a2aInstance(namespace, name string) instances.Instance {
	return instances.Instance{
		Name:            name,
		Namespace:       namespace,
		InboundAddr:     "127.0.0.1:0",
		InboundProtocol: instances.ProtocolA2A,
	}
}

func mcpInstance(namespace, name string) instances.Instance {
	return instances.Instance{
		Name:            name,
		Namespace:       namespace,
		InboundAddr:     "127.0.0.1:0",
		InboundProtocol: instances.ProtocolMCP,
	}
}

// newCortexContext isolates HOME, records the given instances, and makes a
// context named "cortex" current — pointed at an address nothing is listening on,
// which is the whole point: the commands under test must work anyway.
func newCortexContext(t *testing.T, insts ...instances.Instance) {
	t.Helper()
	isolateHome(t)

	for _, inst := range insts {
		if _, err := instances.Create(inst); err != nil {
			t.Fatalf("creating instance %s/%s: %v", inst.Namespace, inst.Name, err)
		}
	}

	if _, err := execute(t, "config", "create-context",
		"--name", "cortex", "--server", deadServerURI, "--namespace", "team1"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
}

// TestCortexContextNeedsNoDaemon is the point of the in-process transport: a
// context named "cortex" answers from handlers in this process, so the commands
// work with nothing listening at its server address.
func TestCortexContextNeedsNoDaemon(t *testing.T) {
	newCortexContext(t, a2aInstance("team1", "bot"), mcpInstance("team1", "shell"))

	out, err := execute(t, "agents", "list")
	if err != nil {
		t.Fatalf("agents list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bot") {
		t.Errorf("agents list is missing the recorded agent:\n%s", out)
	}
	// The mcp instance is a tool, so it must not appear among the agents.
	if strings.Contains(out, "shell") {
		t.Errorf("agents list included an mcp instance:\n%s", out)
	}

	out, err = execute(t, "tools", "list")
	if err != nil {
		t.Fatalf("tools list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "shell") {
		t.Errorf("tools list is missing the recorded tool:\n%s", out)
	}

	// A detail route, which needs the mux's path values to survive the transport.
	out, err = execute(t, "agents", "get", "bot")
	if err != nil {
		t.Fatalf("agents get: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bot") {
		t.Errorf("agents get is missing the agent:\n%s", out)
	}
}

// TestCortexNamespacesAreRealDirectories pins the one intended difference from
// the daemon: `namespaces list` reports the directories that exist on disk, not
// the daemon's hardcoded team1,team2 default.
func TestCortexNamespacesAreRealDirectories(t *testing.T) {
	newCortexContext(t, a2aInstance("team1", "bot"), a2aInstance("research", "other"))

	out, err := execute(t, "namespaces", "list")
	if err != nil {
		t.Fatalf("namespaces list: %v\n%s", err, out)
	}
	// research is the load-bearing name: it is not in the daemon's default list and
	// is not seeded by create-context, so it can only be reported by reading the
	// directories that exist.
	for _, ns := range []string{"team1", "research"} {
		if !strings.Contains(out, ns) {
			t.Errorf("namespaces list is missing %q:\n%s", ns, out)
		}
	}
	// A name with no directory must not appear. team2 cannot serve as this check
	// any more — create-context seeds it — so use one nothing creates.
	if strings.Contains(out, "team9") {
		t.Errorf("namespaces list reported a namespace with no directory:\n%s", out)
	}
}

// TestCortexNamespacesIgnoreTheDaemonDefault is the sharper half of the check
// above: with a cortex context that was never seeded — as a config written before
// create-context seeded namespaces would be — neither name in the daemon's
// hardcoded team1,team2 default may appear unless its directory exists.
func TestCortexNamespacesIgnoreTheDaemonDefault(t *testing.T) {
	isolateHome(t)

	if _, err := instances.Create(a2aInstance("research", "bot")); err != nil {
		t.Fatalf("creating instance: %v", err)
	}
	// Written directly rather than through create-context, which would seed the
	// very directories this test needs to be absent.
	writeCortexContext(t)

	out, err := execute(t, "namespaces", "list")
	if err != nil {
		t.Fatalf("namespaces list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "research") {
		t.Errorf("namespaces list is missing the namespace on disk:\n%s", out)
	}
	for _, ns := range serve.SplitNamespaces(defaultServeNamespaces) {
		if strings.Contains(out, ns) {
			t.Errorf("namespaces list reported %q from the daemon's default; no directory exists for it:\n%s", ns, out)
		}
	}
}

// writeCortexContext saves a current context named "cortex" without going through
// create-context, so no namespace directories are seeded.
func writeCortexContext(t *testing.T) {
	t.Helper()

	path, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	// Load binds the config to path, so Save writes back to it.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Upsert(config.Context{
		Name:      rossoctlclient.CortexContextName,
		Type:      config.TypeAPI,
		Server:    deadServerURI,
		Namespace: "research",
	})
	if err := cfg.SetCurrent(rossoctlclient.CortexContextName); err != nil {
		t.Fatalf("SetCurrent: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// TestCortexEmptyMachine verifies a machine where nothing has ever run is not an
// error: the instances directory does not exist, which reads as empty.
func TestCortexEmptyMachine(t *testing.T) {
	newCortexContext(t)

	out, err := execute(t, "agents", "list")
	if err != nil {
		t.Fatalf("agents list: %v\n%s", err, out)
	}
	if strings.Contains(out, "127.0.0.1:1") {
		t.Errorf("output mentions the server address, suggesting a dial was attempted:\n%s", out)
	}
}

// TestNonCortexContextStillDials proves there is no accidental in-process
// fallback: an identically-configured context under any other name must fail
// against a dead address rather than being answered locally.
func TestNonCortexContextStillDials(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "prod", "--server", deadServerURI, "--namespace", "team1"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if out, err := execute(t, "agents", "list"); err == nil {
		t.Fatalf("agents list succeeded on a non-cortex context with nothing listening:\n%s", out)
	}
}

// TestCortexNameIsExact pins that dispatch is an exact name match rather than a
// substring one, so a context named "my-cortex" dials.
func TestCortexNameIsExact(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "my-cortex", "--server", deadServerURI, "--namespace", "team1"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if out, err := execute(t, "agents", "list"); err == nil {
		t.Fatalf("agents list succeeded for a context named my-cortex:\n%s", out)
	}
}

// TestServerFlagAlwaysDials pins the synthetic context newClient builds for
// --server: it carries no name, so an explicit --server can never be answered
// in-process. That behavior is load-bearing and would otherwise be easy to break
// by accident.
func TestServerFlagAlwaysDials(t *testing.T) {
	newCortexContext(t, a2aInstance("team1", "bot"))

	// A live server whose answer is distinguishable from what the local handlers
	// would produce, so the assertion shows which one replied.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"name":"remote-agent","namespace":"team1",` +
			`"description":"","status":"Running",` +
			`"labels":{"protocol":["a2a"],"framework":null,"type":null},` +
			`"workloadType":null,"createdAt":null}]}`))
	}))
	t.Cleanup(srv.Close)

	out, err := execute(t, "--server", srv.URL+"/api/v1/", "agents", "list")
	if err != nil {
		t.Fatalf("agents list --server: %v\n%s", err, out)
	}
	if hits == 0 {
		t.Error("--server was not dialed; the request was answered in-process")
	}
	if !strings.Contains(out, "remote-agent") {
		t.Errorf("output does not come from the --server target:\n%s", out)
	}
	if strings.Contains(out, "bot") {
		t.Errorf("output contains the local instance, so --server was overridden:\n%s", out)
	}
}

// TestCortexUnimplementedRoutesMatchDaemon pins "do not fix": serve does not
// implement /auth/status, so `status` fails in-process exactly as it does against
// the daemon. Making it work is a change to serve, not to the transport.
func TestCortexUnimplementedRoutesMatchDaemon(t *testing.T) {
	newCortexContext(t)

	out, err := execute(t, "status")
	if err == nil {
		t.Fatalf("status succeeded; serve does not implement /auth/status:\n%s", out)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, want the 500 the daemon returns", err)
	}
}

// TestCortexVerboseNamesTransport verifies --verbose says who answered. This is
// the mitigation for dispatching on the name alone: a context named "cortex"
// pointed at a remote server is answered locally, and -v is how a user finds out.
func TestCortexVerboseNamesTransport(t *testing.T) {
	newCortexContext(t, a2aInstance("team1", "bot"))

	stdout, stderr, err := executeSplit(t, "--verbose", "agents", "list")
	if err != nil {
		t.Fatalf("agents list --verbose: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "in-process") {
		t.Errorf("stderr does not name the in-process transport:\n%s", stderr)
	}
	// Verbose output belongs on stderr, so stdout stays parseable.
	if strings.Contains(stdout, "in-process") {
		t.Errorf("the transport notice leaked into stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "bot") {
		t.Errorf("stdout is missing the results:\n%s", stdout)
	}
}

// TestNonCortexVerboseIsQuietAboutTransport verifies the notice is specific to the
// in-process path: a dialing context must not claim requests stayed local.
func TestNonCortexVerboseIsQuietAboutTransport(t *testing.T) {
	isolateHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := execute(t, "config", "create-context",
		"--name", "prod", "--server", srv.URL+"/api/v1/", "--namespace", "team1"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	_, stderr, err := executeSplit(t, "--verbose", "agents", "list")
	if err != nil {
		t.Fatalf("agents list --verbose: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "in-process") {
		t.Errorf("a dialing context claimed to be served in-process:\n%s", stderr)
	}
}

// TestCreateCortexContextSeedsNamespaces verifies creating the cortex context also
// creates the default namespace directories, so the new context has somewhere to
// start agents and tools and `namespaces list` reports them straight away.
func TestCreateCortexContextSeedsNamespaces(t *testing.T) {
	isolateHome(t)

	out, err := execute(t, "config", "create-context",
		"--name", "cortex", "--server", deadServerURI, "--namespace", "team1")
	if err != nil {
		t.Fatalf("create-context: %v\n%s", err, out)
	}

	want := serve.SplitNamespaces(defaultServeNamespaces)
	for _, ns := range want {
		dir, err := instances.Dir(ns)
		if err != nil {
			t.Fatalf("Dir(%q): %v", ns, err)
		}
		info, statErr := os.Stat(dir)
		if statErr != nil {
			t.Errorf("namespace %q was not created: %v", ns, statErr)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
		// Named on stdout, so the user knows what was created on their behalf.
		if !strings.Contains(out, ns) {
			t.Errorf("output does not mention namespace %q:\n%s", ns, out)
		}
	}

	// The seeded namespaces are immediately visible through the in-process
	// transport, which is what makes them selectable.
	listed, err := execute(t, "namespaces", "list")
	if err != nil {
		t.Fatalf("namespaces list: %v\n%s", err, listed)
	}
	for _, ns := range want {
		if !strings.Contains(listed, ns) {
			t.Errorf("namespaces list is missing the seeded %q:\n%s", ns, listed)
		}
	}
}

// TestCreateNonCortexContextSeedsNothing verifies the seeding is specific to the
// cortex context: any other name creates no directories, since a remote server's
// namespaces are not this machine's to invent.
func TestCreateNonCortexContextSeedsNothing(t *testing.T) {
	isolateHome(t)

	out, err := execute(t, "config", "create-context",
		"--name", "prod", "--server", deadServerURI, "--namespace", "team1")
	if err != nil {
		t.Fatalf("create-context: %v\n%s", err, out)
	}

	base, err := instances.BaseDir()
	if err != nil {
		t.Fatalf("BaseDir: %v", err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Errorf("creating a non-cortex context created %s (stat err: %v)", base, err)
	}
}

// TestCreateCortexContextIsRepeatable verifies re-creating the cortex context over
// an existing one succeeds and leaves recorded instances intact, since
// create-context replaces a context by design.
func TestCreateCortexContextIsRepeatable(t *testing.T) {
	newCortexContext(t, a2aInstance("team1", "bot"))

	out, err := execute(t, "config", "create-context",
		"--name", "cortex", "--server", deadServerURI, "--namespace", "team2")
	if err != nil {
		t.Fatalf("second create-context: %v\n%s", err, out)
	}

	listed, err := execute(t, "agents", "list", "--namespace", "team1")
	if err != nil {
		t.Fatalf("agents list: %v\n%s", err, listed)
	}
	if !strings.Contains(listed, "bot") {
		t.Errorf("re-creating the context lost the recorded instance:\n%s", listed)
	}
}

// TestCortexHelpDocumentsTheRule verifies the naming rule is discoverable from the
// help, since nothing in a context's stored fields reveals it.
func TestCortexHelpDocumentsTheRule(t *testing.T) {
	for _, args := range [][]string{
		{"cortex", "--help"},
		{"cortex", "serve", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateHome(t)

			out, err := execute(t, args...)
			if err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			if !strings.Contains(out, `"cortex"`) {
				t.Errorf("help does not mention the cortex context name:\n%s", out)
			}
		})
	}
}
