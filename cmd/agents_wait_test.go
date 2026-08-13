package cmd

import (
	"strings"
	"testing"
	"time"
)

const agentsWaitPath = "/api/v1/agents/team1/orders"

// setupWaitContext points the current context at srv with namespace team1.
func setupWaitContext(t *testing.T, srv *waitServer) {
	t.Helper()
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	if _, err := execute(t, "config", "set-context", "--namespace", "team1"); err != nil {
		t.Fatalf("set-context: %v", err)
	}
}

func TestAgentsWaitReturnsImmediatelyWhenReady(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath, ready("Ready"))
	setupWaitContext(t, srv)

	// A long interval with no shortening: if the loop slept before its first
	// probe, this test would take 10s rather than milliseconds. Combined with the
	// request count, that pins probe-before-sleep.
	fastWaitPolling(t, 10*time.Second)

	start := time.Now()
	out, err := execute(t, "agents", "wait", "orders")
	if err != nil {
		t.Fatalf("agents wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v for an already-ready agent; want an immediate return", elapsed)
	}
	if got := srv.requests(); got != 1 {
		t.Errorf("polled %d times, want 1", got)
	}
	if !strings.Contains(out, "ready") {
		t.Errorf("output %q does not report readiness", out)
	}
}

func TestAgentsWaitPollsUntilReady(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath,
		ready("Not Ready"), ready("Progressing"), ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	if _, err := execute(t, "agents", "wait", "orders", "--timeout", "300ms"); err != nil {
		t.Fatalf("agents wait: %v", err)
	}
	// Exactly three: one per scripted status. Fewer means a transient status was
	// mistaken for ready; more means the ready status was not recognized.
	if got := srv.requests(); got != 3 {
		t.Errorf("polled %d times, want 3", got)
	}
}

// TestAgentsWaitTreatsRunningAsReady covers the in-process cortex, which reports
// "Running" for every instance rather than "Ready" (serve.instanceStatus). No
// other test produces that value, and without it a wait against a cortex context
// would run to its timeout.
func TestAgentsWaitTreatsRunningAsReady(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath, ready("Running"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	if _, err := execute(t, "agents", "wait", "orders", "--timeout", "50ms"); err != nil {
		t.Fatalf("agents wait: %v", err)
	}
	if got := srv.requests(); got != 1 {
		t.Errorf("polled %d times, want 1", got)
	}
}

func TestAgentsWaitFailsFastOnFailed(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath, ready("Progressing"), ready("Failed"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	// Long enough relative to the 1ms poll interval that reaching it would take
	// hundreds of polls, so the request count below distinguishes a fail-fast from
	// a timeout — but short enough that a version which kept polling reports in a
	// moment instead of stalling the suite for half a minute.
	out, err := execute(t, "agents", "wait", "orders", "--timeout", "300ms")
	if err == nil {
		t.Fatal("agents wait succeeded on a Failed agent; want an error")
	}
	// Exactly two: the command must stop at the Failed status rather than poll on
	// and eventually report a timeout, which would also be an error.
	if got := srv.requests(); got != 2 {
		t.Errorf("polled %d times, want 2 (stop at the failure)", got)
	}
	if !strings.Contains(err.Error(), "Failed") {
		t.Errorf("error %q does not name the status", err)
	}
	if strings.Contains(err.Error(), "did not become ready within") {
		t.Errorf("error %q reports a timeout; want the terminal status", err)
	}
	_ = out
}

func TestAgentsWaitTimesOutAndReportsLastStatus(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath, ready("Building"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	_, err := execute(t, "agents", "wait", "orders", "--timeout", "30ms")
	if err == nil {
		t.Fatal("agents wait succeeded on an agent that never became ready")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("error %q does not report a timeout", err)
	}
	// The last status is what distinguishes "still building" from "stuck", and it
	// is invisible once the command has exited.
	if !strings.Contains(err.Error(), "Building") {
		t.Errorf("error %q does not name the last observed status", err)
	}
}

// TestAgentsWaitAbortsOn404 pins that a 404 is fatal at any point in the wait,
// not merely on the first probe: the server denies the resource exists, which no
// amount of waiting changes.
func TestAgentsWaitAbortsOn404(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath, ready("Not Ready"), failing(404))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	// Short for the same reason as the fail-fast test above: a version that
	// retried the 404 must fail on the count, promptly.
	_, err := execute(t, "agents", "wait", "orders", "--timeout", "300ms")
	if err == nil {
		t.Fatal("agents wait succeeded against a 404; want an error")
	}
	// Stops at the 404 rather than retrying until the timeout.
	if got := srv.requests(); got != 2 {
		t.Errorf("polled %d times, want 2 (stop at the 404)", got)
	}
	if strings.Contains(err.Error(), "did not become ready within") {
		t.Errorf("error %q reports a timeout; want the server's 404", err)
	}
}

// TestAgentsWaitToleratesTransientServerErrors scopes the abort above to 404
// only. A 5xx describes the path to the server, not the resource, so the wait
// continues and the timeout bounds it.
func TestAgentsWaitToleratesTransientServerErrors(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath, failing(500), failing(503), ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	if _, err := execute(t, "agents", "wait", "orders", "--timeout", "30s"); err != nil {
		t.Fatalf("agents wait: %v", err)
	}
	if got := srv.requests(); got != 3 {
		t.Errorf("polled %d times, want 3", got)
	}
}

func TestAgentsWaitEmptyReadyStatusKeepsWaiting(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath, ready(""), ready(""), ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	if _, err := execute(t, "agents", "wait", "orders", "--timeout", "30s"); err != nil {
		t.Fatalf("agents wait: %v", err)
	}
	if got := srv.requests(); got != 3 {
		t.Errorf("polled %d times, want 3", got)
	}
}

// TestAgentsWaitUnknownStatusKeepsWaiting pins the default arm of the
// classifier: a status this build does not recognize must degrade to a bounded
// timeout, not a verdict of failure.
func TestAgentsWaitUnknownStatusKeepsWaiting(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath, ready("Rebalancing"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	_, err := execute(t, "agents", "wait", "orders", "--timeout", "30ms")
	if err == nil {
		t.Fatal("agents wait succeeded on an unrecognized status")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("error %q is not a timeout; an unknown status must not fail fast", err)
	}
	// More than one poll proves it kept waiting rather than deciding immediately.
	if got := srv.requests(); got < 2 {
		t.Errorf("polled %d times, want more than 1", got)
	}
}

func TestAgentsWaitZeroTimeoutWaitsIndefinitely(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath,
		ready("Not Ready"), ready("Not Ready"), ready("Not Ready"),
		ready("Not Ready"), ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	// --timeout 0 must not mean "no wait"; it must outlast every non-ready poll.
	if _, err := execute(t, "agents", "wait", "orders", "--timeout", "0"); err != nil {
		t.Fatalf("agents wait --timeout 0: %v", err)
	}
	if got := srv.requests(); got != 5 {
		t.Errorf("polled %d times, want 5", got)
	}
}

func TestAgentsWaitUsesNamespaceFlag(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, "/api/v1/agents/team2/orders", ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	// The group's --namespace must reach the leaf; the server errors on any other
	// path, so a wait against team1 fails the test. --timeout keeps that failure
	// from retrying the resulting 500s for the full default minute first.
	if _, err := execute(t, "agents", "--namespace", "team2", "wait", "orders", "--timeout", "50ms"); err != nil {
		t.Fatalf("agents wait: %v", err)
	}
	if got := srv.requests(); got != 1 {
		t.Errorf("polled %d times, want 1", got)
	}
}

func TestAgentsWaitSuccessLineNamesResource(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath, ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	out, err := execute(t, "agents", "wait", "orders", "--timeout", "50ms")
	if err != nil {
		t.Fatalf("agents wait: %v", err)
	}
	for _, want := range []string{"orders", "team1", "ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain %q", out, want)
		}
	}
}

func TestAgentsWaitTakesExactlyOneName(t *testing.T) {
	isolateHome(t)

	if _, err := execute(t, "agents", "wait"); err == nil {
		t.Error("agents wait with no name succeeded; want an error")
	}
	if _, err := execute(t, "agents", "wait", "orders", "extra"); err == nil {
		t.Error("agents wait with two names succeeded; want an error")
	}
}

// TestAgentsWaitVerboseAnnouncesOnce pins all three properties of the progress
// message: it is announced once however many polls follow, it lands on stderr so
// a wait inside a pipeline does not pollute stdout, and stdout carries only the
// success line.
func TestAgentsWaitVerboseAnnouncesOnce(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath,
		ready("Not Ready"), ready("Not Ready"), ready("Not Ready"), ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	stdout, stderr, err := executeSplit(t, "agents", "wait", "orders", "-v")
	if err != nil {
		t.Fatalf("agents wait -v: %v", err)
	}
	if n := strings.Count(stderr, "waiting for agent"); n != 1 {
		t.Errorf("announced %d times on stderr, want 1:\n%s", n, stderr)
	}
	if strings.Contains(stdout, "waiting for agent") {
		t.Errorf("progress message reached stdout: %q", stdout)
	}
	if !strings.Contains(stdout, "is ready") {
		t.Errorf("stdout %q does not carry the success line", stdout)
	}
}

func TestAgentsWaitQuietByDefault(t *testing.T) {
	isolateHome(t)
	srv := newWaitServer(t, agentsWaitPath,
		ready("Not Ready"), ready("Not Ready"), ready("Ready"))
	setupWaitContext(t, srv)
	fastWaitPolling(t, time.Millisecond)

	stdout, stderr, err := executeSplit(t, "agents", "wait", "orders")
	if err != nil {
		t.Fatalf("agents wait: %v", err)
	}
	// Without --verbose a successful wait says nothing but its result.
	if stderr != "" {
		t.Errorf("stderr is not empty without --verbose: %q", stderr)
	}
	if !strings.Contains(stdout, "is ready") {
		t.Errorf("stdout %q does not carry the success line", stdout)
	}
}
