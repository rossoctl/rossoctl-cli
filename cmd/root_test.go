package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

// TestMain isolates HOME to a throwaway directory for the whole cmd test
// binary, so no test can create or mutate the real
// ~/.config/rossoctl/config.yaml when a command resolves its server via the
// context config. XDG_CONFIG_HOME and ROSSOCORTEX_CONFIG_DIR are cleared so the
// default paths resolve under the temp HOME rather than a developer's real
// config dir.
//
// It also repoints `authbridge exec`'s --logfile default into that directory. The
// registered default is the real /tmp/authbridge.log, and resetFlags restores
// every flag to its default between runs, so without this every exec test would
// append to a developer's actual logfile.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rossoctl-cmd-test-home")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", dir)
	_ = os.Unsetenv("XDG_CONFIG_HOME")
	_ = os.Unsetenv("ROSSOCORTEX_CONFIG_DIR")

	// Retarget the --logfile default before any test runs. DefValue is what
	// resetFlags restores to, so both it and the bound value must change.
	if f := authbridgeExecCmd.Flags().Lookup("logfile"); f != nil {
		f.DefValue = filepath.Join(dir, "authbridge.log")
		_ = f.Value.Set(f.DefValue)
	}

	// Never spawn a real browser during the device-login tests, and don't
	// actually sleep between token polls.
	browserOpener = func(string) error { return nil }
	deviceflowSleep = func(time.Duration) {}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// execute runs the given args against the root command tree and returns
// whatever was written to stdout/stderr plus any error. Cobra shares global
// command state, so tests capture output via SetOut rather than relying on
// os.Stdout.
func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()

	// Cobra flag values are stored in package-level globals bound once in
	// init(); they persist across Execute calls in the same test binary.
	// Reset every flag to its default so tests don't leak state into each
	// other (e.g. --json set by one test affecting the next).
	resetFlags(rootCmd)

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	err := rootCmd.Execute()
	return buf.String(), err
}

// executeSplit is like execute but captures stdout and stderr separately, so
// tests can assert that verbose logging lands on stderr without polluting the
// stdout results (e.g. --json).
func executeSplit(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	resetFlags(rootCmd)

	var outBuf, errBuf bytes.Buffer
	rootCmd.SetOut(&outBuf)
	rootCmd.SetErr(&errBuf)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	err = rootCmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

// flagSliceDefaults snapshots the initial value of each slice flag (keyed by
// flag pointer) the first time resetFlags sees it, so subsequent resets can
// restore the real registered default. This is needed because a StringSlice's
// DefValue is a bracketed string that does not round-trip through Value.Set.
var flagSliceDefaults = map[*pflag.Flag][]string{}

// resetFlags restores flag state between Execute calls in the same test
// binary. Flag values are stored in package-level globals bound once in
// init(); Cobra never resets them, so a flag set by one test would otherwise
// leak into the next.
//
// Scalar flags are restored via Set(DefValue). Slice flags (pflag StringSlice)
// need SliceValue.Replace with the snapshotted default: their Set appends after
// the first call, and neither Set("[]") nor Set(DefValue) round-trips. Every
// flag's Changed bit is then cleared.
//
// One hazard this does not fully undo. Whether a slice or array flag's Set
// replaces or appends is gated on a bit private to the pflag value, distinct
// from the Flag.Changed set below, and Replace restores the contents without
// clearing it. So the first --flag in a later test appends to the restored
// default rather than replacing it. Reaching that bit means re-registering the
// flag, which this helper cannot do.
//
// This is survivable only because every such flag either has a nil default
// (--envVar: appending to nil is indistinguishable from replacing) or is only
// ever set to a full replacement in tests (--ports). Give one a non-nil
// default, or drop the Replace above, and values accumulate across Execute
// calls. Either way several --envVar tests fail, because they assert exact
// request-body contents rather than just presence;
// TestAgentsImportEnvVarIsRepeatableAcrossRuns is the one that exists for this
// reason alone.
//
// A flag set also remembers where a "--" delimiter appeared, which
// ArgsLenAtDash reports and `authbridge exec` relies on. That position is private
// state whose only reset is FlagSet.Init, so Init is re-invoked with the set's
// existing name and error-handling policy. Without this, a test that passes
// "--" leaves the delimiter "set" for every later test.
func resetFlags(cmd *cobra.Command) {
	clear := func(f *pflag.Flag) {
		if sv, isSlice := f.Value.(pflag.SliceValue); isSlice {
			def, seen := flagSliceDefaults[f]
			if !seen {
				// First sight: this is the registered default.
				def = append([]string(nil), sv.GetSlice()...)
				flagSliceDefaults[f] = def
			}
			_ = sv.Replace(def)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	}
	cmd.Flags().VisitAll(clear)
	cmd.PersistentFlags().VisitAll(clear)
	if fs := cmd.Flags(); fs.ArgsLenAtDash() != -1 {
		fs.Init(fs.Name(), pflag.ContinueOnError)
	}
	for _, sub := range cmd.Commands() {
		resetFlags(sub)
	}
}

func TestVersionCommand(t *testing.T) {
	out, err := execute(t, "version")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "rossoctl") {
		t.Errorf("version output = %q, want it to contain %q", out, "rossoctl")
	}
}

// unimplementedCommands lists every documented leaf command, addressed by its
// full path from the root. Each must print UNIMPLEMENTED and exit without
// error.
// It is currently empty: every documented leaf is implemented, and the only
// remaining stubs are subcommands with their own coverage (`agents import
// from-source`, `tools import from-source`). The table and its test stay so the
// next stub added is covered without rebuilding them.
var unimplementedCommands = [][]string{
	// "install" is implemented (prints setup instructions); tested in install_test.go.
	// "login" is implemented (sets the current context's token); tested in login_test.go.
	// "status" is implemented (auth + platform status); tested in status_test.go.
	// "agents chat" is implemented (streams an A2A message to a named agent);
	// tested in agents_chat_test.go.
	// "agents delete" is implemented (DELETE /agents/<ns>/<name>); tested in
	// agents_delete_test.go.
	// "agents import" has its own from-image/from-source subcommands (tested
	// in agents_import_test.go).
	// "agents list" is implemented (fetches GET /agents) and tested separately.
	// tools list/get/delete and import from-image are implemented; tested in
	// tools_test.go / tools_get_test.go / tools_import_test.go.
	// "ui open" is implemented (opens the context server's site root in a
	// browser); tested in ui_test.go.
}

// TestUnimplementedDescriptionsPrefixed verifies that stub commands advertise
// their status in subcommand listings: their Short description begins with
// "UNIMPLEMENTED", while implemented commands do not.
func TestUnimplementedDescriptionsPrefixed(t *testing.T) {
	// A stub: `agents import from-source`.
	if c, _, _ := rootCmd.Find([]string{"agents", "import", "from-source"}); c == nil {
		t.Fatal("agents import from-source not found")
	} else if !strings.HasPrefix(c.Short, "UNIMPLEMENTED") {
		t.Errorf("stub `agents import from-source` Short = %q, want UNIMPLEMENTED prefix", c.Short)
	}

	// An implemented command must NOT be prefixed.
	if c, _, _ := rootCmd.Find([]string{"agents", "list"}); c == nil {
		t.Fatal("agents list not found")
	} else if strings.HasPrefix(c.Short, "UNIMPLEMENTED") {
		t.Errorf("implemented `agents list` Short = %q, should not be prefixed", c.Short)
	}

	// The subcommand listing containing a stub must surface the prefix. That is
	// `rossoctl agents import` rather than `rossoctl agents`: every leaf directly
	// under the agents group is implemented now, and the remaining stub is a
	// level deeper.
	out, err := execute(t, "agents", "import")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "UNIMPLEMENTED:") {
		t.Errorf("`agents import` help does not surface UNIMPLEMENTED status:\n%s", out)
	}
}

func TestUnimplementedCommandsPrintPlaceholder(t *testing.T) {
	for _, path := range unimplementedCommands {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			out, err := execute(t, path...)
			if err != nil {
				t.Fatalf("%v: unexpected error: %v", path, err)
			}
			if !strings.Contains(out, "UNIMPLEMENTED") {
				t.Errorf("%v output = %q, want it to contain %q", path, out, "UNIMPLEMENTED")
			}
		})
	}
}

// TestGroupsAreNotRunnable verifies that group commands (agents, config, ...)
// show help instead of executing an UNIMPLEMENTED stub when invoked with no
// subcommand. The help listing may mention "UNIMPLEMENTED:" in a subcommand's
// description, so we check that the standalone placeholder line was not
// printed rather than that the substring is absent.
func TestGroupsAreNotRunnable(t *testing.T) {
	groups := []string{"agents", "authbridge", "config", "namespaces", "tools", "ui"}
	for _, g := range groups {
		t.Run(g, func(t *testing.T) {
			out, err := execute(t, g)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", g, err)
			}
			for line := range strings.SplitSeq(out, "\n") {
				if strings.TrimSpace(line) == "UNIMPLEMENTED" {
					t.Errorf("%s executed a stub (printed UNIMPLEMENTED); expected help output", g)
				}
			}
			if !strings.Contains(out, "Available Commands") {
				t.Errorf("%s output = %q, want help with %q", g, out, "Available Commands")
			}
		})
	}
}

// TestErrorHintSuggestsLoginOn401 verifies a 401 from any command that reaches
// the API gets the sign-in hint. This is the case from issue #21: `agents list`
// printed only the raw 401 with no indication that logging in would fix it.
func TestErrorHintSuggestsLoginOn401(t *testing.T) {
	err := &apiclient.StatusError{
		Endpoint:   "http://rossoctl-ui.localtest.me:8080/api/v1/agents?namespace=team1",
		StatusCode: http.StatusUnauthorized,
		Body:       `{"detail":"Token signing key not found"}`,
	}

	hint := errorHint(err)
	if hint == "" {
		t.Fatal("a 401 should produce a hint")
	}
	if !strings.Contains(hint, "rossoctl login") {
		t.Errorf("hint %q should name `rossoctl login`", hint)
	}
}

// TestErrorHintFindsWrappedStatusError verifies the hint survives wrapping, since
// commands add context to client errors on the way up.
func TestErrorHintFindsWrappedStatusError(t *testing.T) {
	inner := &apiclient.StatusError{Endpoint: "http://x/api/v1/tools", StatusCode: http.StatusUnauthorized, Body: "nope"}
	wrapped := fmt.Errorf("listing tools in namespace %q: %w", "team1", inner)

	if hint := errorHint(wrapped); hint == "" {
		t.Error("a wrapped 401 should still produce a hint")
	}
}

// TestErrorHintQuietForOtherErrors verifies the hint is offered only where it is
// the actual remedy. A 403 is the notable exclusion: that is an authenticated
// identity without permission, so signing in again changes nothing.
func TestErrorHintQuietForOtherErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"403 is permission, not credentials", &apiclient.StatusError{StatusCode: http.StatusForbidden, Body: "forbidden"}},
		{"404", &apiclient.StatusError{StatusCode: http.StatusNotFound, Body: "no such agent"}},
		{"500", &apiclient.StatusError{StatusCode: http.StatusInternalServerError, Body: "boom"}},
		{"not an HTTP error at all", errors.New("connection refused")},
		{"nil", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if hint := errorHint(tc.err); hint != "" {
				t.Errorf("errorHint = %q, want no hint", hint)
			}
		})
	}
}
