package cmd

import (
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
