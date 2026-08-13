package cmd

import (
	"strings"
	"testing"
	"time"
)

// The wait loop itself is covered by the agents tests; ToolDetail is an alias of
// AgentDetail, so these cover only what differs: the endpoint, the tools group's
// --namespace flag, and the build statuses only tools report.

// TestToolsWaitUsesToolsEndpoint is the one test the type alias makes necessary:
// a tools leaf that called GetAgent would compile and type-check, because both
// methods return the same type. Only the requested path reveals the mistake.
func TestToolsWaitUsesToolsEndpoint(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, "/api/v1/tools/team1/weather", ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	// A short timeout so a leaf wired to the wrong endpoint fails in milliseconds.
	// Every request would then land on the server's default arm, which answers 500
	// — an error the wait deliberately retries, so with the default 60s timeout
	// this test would take a minute to report the mistake.
	if _, err := execute(t, "tools", "wait", "weather", "--timeout", "50ms"); err != nil {
		t.Fatalf("tools wait: %v", err)
	}
	if got := srv.requests(); got != 1 {
		t.Errorf("polled %d times, want 1", got)
	}
}

// TestToolsWaitFailsFastOnBuildFailed covers the two-word status a source build
// reports on failure — easy to write as one word, and only tools produce it.
func TestToolsWaitFailsFastOnBuildFailed(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, "/api/v1/tools/team1/weather", ready("Build Failed"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	// Long enough that the failure below cannot be a timeout, short enough that a
	// leaf which kept polling reports in a moment rather than in half a minute.
	_, err := execute(t, "tools", "wait", "weather", "--timeout", "300ms")
	if err == nil {
		t.Fatal("tools wait succeeded on a Build Failed tool; want an error")
	}
	if got := srv.requests(); got != 1 {
		t.Errorf("polled %d times, want 1 (stop at the failure)", got)
	}
	if !strings.Contains(err.Error(), "Build Failed") {
		t.Errorf("error %q does not name the status", err)
	}
	// The error must be about the status, not about time running out.
	if strings.Contains(err.Error(), "did not become ready within") {
		t.Errorf("error %q reports a timeout; want the terminal status", err)
	}
}

// TestToolsWaitKeepsWaitingWhileBuilding pins that a build in flight is a
// pending state, distinct from the Build Failed above.
func TestToolsWaitKeepsWaitingWhileBuilding(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, "/api/v1/tools/team1/weather",
		ready("Building"), ready("Building"), ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	if _, err := execute(t, "tools", "wait", "weather", "--timeout", "300ms"); err != nil {
		t.Fatalf("tools wait: %v", err)
	}
	if got := srv.requests(); got != 3 {
		t.Errorf("polled %d times, want 3", got)
	}
}

func TestToolsWaitUsesNamespaceFlag(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, "/api/v1/tools/team2/weather", ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	// --timeout keeps a wait against the wrong namespace from retrying the
	// resulting 500s for the full default minute before the test can report it.
	if _, err := execute(t, "tools", "--namespace", "team2", "wait", "weather", "--timeout", "50ms"); err != nil {
		t.Fatalf("tools wait: %v", err)
	}
	if got := srv.requests(); got != 1 {
		t.Errorf("polled %d times, want 1", got)
	}
}

func TestToolsWaitSuccessLineNamesTool(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, "/api/v1/tools/team1/weather", ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	out, err := execute(t, "tools", "wait", "weather", "--timeout", "50ms")
	if err != nil {
		t.Fatalf("tools wait: %v", err)
	}
	// "Tool", not "Agent" — a copy-pasted success line is otherwise invisible.
	if !strings.Contains(out, "Tool") {
		t.Errorf("output %q does not name the resource kind", out)
	}
	for _, want := range []string{"weather", "team1", "ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain %q", out, want)
		}
	}
}

func TestToolsWaitTakesExactlyOneName(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "tools", "wait"); err == nil {
		t.Error("tools wait with no name succeeded; want an error")
	}
	if _, err := execute(t, "tools", "wait", "weather", "extra"); err == nil {
		t.Error("tools wait with two names succeeded; want an error")
	}
}
