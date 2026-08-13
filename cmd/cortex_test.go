package cmd

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/serve"
)

// loadTestConfig reads the config written under the isolated HOME so tests can
// assert on the persisted contexts.
func loadTestConfig(t *testing.T) *config.Config {
	t.Helper()
	path, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// cortexTestContextName re-exports config.CortexContextName for test files where
// "config" names authbridge's package instead of rossoctl's.
const cortexTestContextName = config.CortexContextName

// writeContextConfig writes a config holding exactly the named context, and
// nothing else, as the config under the isolated HOME.
//
// Tests use it to build a precise starting state that no CLI command can produce:
// `config create-context` seeds a cortex context of its own, so it cannot set up
// a config that deliberately lacks one.
//
// Takes strings rather than a config.Context so callers in files where "config"
// names authbridge's package can use it without importing rossoctl's too.
func writeContextConfig(t *testing.T, name, server string) {
	t.Helper()
	path, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	// Load binds the config to path — Save needs that, and it is unexported, so a
	// literal &config.Config{} cannot be saved. A missing file loads as empty.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	cfg.Contexts = []config.Context{{Name: name, Server: server}}
	cfg.CurrentContext = ""
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// findTestContext reports the named context from cfg, and whether it is there.
//
// Distinct from cfg.Get only in taking the loaded config as a value, so a test
// can assert against a snapshot taken before the command ran.
func findTestContext(cfg *config.Config, name string) (config.Context, bool) {
	for _, ctx := range cfg.Contexts {
		if ctx.Name == name {
			return ctx, true
		}
	}
	return config.Context{}, false
}

// The cortex group's visibility in help listings is covered by
// TestCortexGroupVisible in authbridge_exec_test.go.

// TestCortexRemovedSubcommands verifies doctor, start, status, and stop are gone
// from the command tree.
//
// The check is on registration rather than on an exit code: the cortex group has
// no RunE of its own, so an unknown subcommand makes it print help and succeed
// rather than fail.
func TestCortexRemovedSubcommands(t *testing.T) {
	cortex, _, err := rootCmd.Find([]string{"cortex"})
	if err != nil {
		t.Fatalf("cortex not found: %v", err)
	}

	registered := map[string]bool{}
	for _, c := range cortex.Commands() {
		registered[c.Name()] = true
	}

	for _, sub := range []string{"doctor", "start", "status", "stop"} {
		if registered[sub] {
			t.Errorf("cortex %s should no longer be registered", sub)
		}
	}
	if !registered["serve"] {
		t.Error("cortex serve should be registered")
	}
}

// TestCortexServeIsRegistered verifies `cortex serve` exists and documents its
// defaults, without starting a server.
func TestCortexServeIsRegistered(t *testing.T) {
	isolateHome(t)

	out, err := execute(t, "cortex", "serve", "--help")
	if err != nil {
		t.Fatalf("cortex serve --help: %v", err)
	}
	if !strings.Contains(out, defaultServeAddress) {
		t.Errorf("expected help to document the default address %q:\n%s", defaultServeAddress, out)
	}
	if !strings.Contains(out, defaultServeNamespaces) {
		t.Errorf("expected help to document the default namespaces %q:\n%s", defaultServeNamespaces, out)
	}
}

// TestDefaultServeAddressAvoidsAuthbridgePorts verifies the default serve port
// does not collide with the ports `authbridge exec` publishes. A collision there
// fails the bind, or worse reaches AuthBridge instead of this server.
func TestDefaultServeAddressAvoidsAuthbridgePorts(t *testing.T) {
	_, _, err := serve.SplitAddress(defaultServeAddress)
	if err != nil {
		t.Fatalf("the default address must parse: %v", err)
	}
	for _, taken := range []string{":8000", ":8081", ":9093", ":9094"} {
		if strings.HasSuffix(defaultServeAddress, taken) {
			t.Errorf("default address %q uses port %s, which authbridge exec publishes",
				defaultServeAddress, strings.TrimPrefix(taken, ":"))
		}
	}
}

// TestDefaultServeNamespacesParse verifies the default --namespaces value parses
// into the two namespaces it names.
func TestDefaultServeNamespacesParse(t *testing.T) {
	got := serve.SplitNamespaces(defaultServeNamespaces)
	if want := []string{"team1", "team2"}; !slices.Equal(got, want) {
		t.Errorf("SplitNamespaces(%q) = %v, want %v", defaultServeNamespaces, got, want)
	}
}

// TestCortexServeRejectsBadAddress verifies an unusable --address fails before
// any listener is bound, so the command returns instead of blocking.
func TestCortexServeRejectsBadAddress(t *testing.T) {
	for _, addr := range []string{
		"http://localhost:9093/api/v1", // a scheme is rejected, not ignored
		"localhost",                    // no port
	} {
		t.Run(addr, func(t *testing.T) {
			isolateHome(t)

			if _, err := execute(t, "cortex", "serve", "--address", addr); err == nil {
				t.Errorf("cortex serve --address %q should error", addr)
			}
		})
	}
}

// TestCortexServeTakesNoArgs verifies positional arguments are rejected rather
// than silently ignored.
func TestCortexServeTakesNoArgs(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "cortex", "serve", "extra"); err == nil {
		t.Error("cortex serve should reject positional arguments")
	}
}

// TestCortexServeRejectsBadAddressBeforeTouchingConfig pins the ordering: the
// context is ensured only after a successful bind, so an invocation that cannot
// serve does not repoint the user's current context on its way out.
func TestCortexServeRejectsBadAddressBeforeTouchingConfig(t *testing.T) {
	path := isolateHome(t)

	if _, err := execute(t, "cortex", "serve", "--address", "localhost"); err == nil {
		t.Fatal("cortex serve --address localhost should error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a failed serve wrote %s; the context must be left alone (stat err: %v)", path, err)
	}
}

// TestEnsureCortexContextMakesCurrent covers the makeCurrent=true half of the
// helper, which is what `cortex serve` uses: an existing current context is
// replaced, so rossoctl's own commands reach the cortex just started.
func TestEnsureCortexContextMakesCurrent(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", "mine", "--server", "http://mine/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if err := ensureCortexContext(rootCmd, true); err != nil {
		t.Fatalf("ensureCortexContext: %v", err)
	}

	cfg := loadTestConfig(t)
	if cfg.CurrentContext != config.CortexContextName {
		t.Errorf("current context = %q, want %q", cfg.CurrentContext, config.CortexContextName)
	}
	// The context it replaced as current must still exist, so switching back is
	// possible.
	if _, ok := findTestContext(cfg, "mine"); !ok {
		t.Error(`the "mine" context is gone; making cortex current must not remove it`)
	}
}

// TestEnsureCortexContextPreservesExistingCortex pins that an already-configured
// cortex context is not overwritten. A user who set a namespace on it, or pointed
// it at a non-default address for `cortex serve --address`, keeps both.
func TestEnsureCortexContextPreservesExistingCortex(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "config", "create-context",
		"--name", config.CortexContextName,
		"--server", "http://localhost:9999/api/v1/", "--namespace", "mine"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if err := ensureCortexContext(rootCmd, true); err != nil {
		t.Fatalf("ensureCortexContext: %v", err)
	}

	cortex, ok := findTestContext(loadTestConfig(t), config.CortexContextName)
	if !ok {
		t.Fatal("the cortex context is gone")
	}
	if cortex.Namespace != "mine" {
		t.Errorf("namespace = %q, want %q; an existing cortex context must not be reset",
			cortex.Namespace, "mine")
	}
	if cortex.Server != "http://localhost:9999/api/v1/" {
		t.Errorf("server = %q; an existing cortex context must not be reset", cortex.Server)
	}
}

// TestEnsureCortexContextDoesNotElectWithoutMakeCurrent is the assertion that
// distinguishes exec's use of the helper from serve's. With makeCurrent false the
// context is created, and the current-context reference is left exactly as it was
// — including left unset on a fresh machine, rather than defaulting to the one
// context that now exists.
func TestEnsureCortexContextDoesNotElectWithoutMakeCurrent(t *testing.T) {
	isolateHome(t)

	if err := ensureCortexContext(rootCmd, false); err != nil {
		t.Fatalf("ensureCortexContext: %v", err)
	}

	cfg := loadTestConfig(t)
	if _, ok := findTestContext(cfg, config.CortexContextName); !ok {
		t.Error("the cortex context was not created")
	}
	if cfg.CurrentContext != "" {
		t.Errorf("current context = %q, want unset", cfg.CurrentContext)
	}
}
