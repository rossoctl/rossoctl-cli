package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

// waitPollInterval is the period between readiness probes.
//
// A variable rather than a constant so tests can shorten it; nothing exposes it
// as a flag, because no user has a reason to want a different value and a knob
// that only tests turn is better left out of the interface.
var waitPollInterval = 2 * time.Second

// defaultWaitTimeout bounds a wait that does not say otherwise. A wait is capped
// by default because the usual reason readiness never arrives is a workload that
// is not coming back, and an unbounded wait in CI would hang the job instead of
// failing it. --timeout 0 opts out.
const defaultWaitTimeout = 60 * time.Second

// readyFetcher fetches the current state of the resource being waited on.
//
// A function rather than an interface because agents and tools are read through
// different methods (GetAgent, GetTool) that return the same type: ToolDetail is
// an alias of AgentDetail, so one wait loop serves both once the fetch is
// abstracted. Each leaf supplies a closure over its own client and namespace.
type readyFetcher func(context.Context) (*apiclient.AgentDetail, error)

// readyState is what a wait should do about an observed readyStatus.
type readyState int

const (
	// statePending means readiness has not been reached and may still arrive.
	statePending readyState = iota
	// stateReady means the resource is serving.
	stateReady
	// stateFailed means readiness will never arrive without intervention.
	stateFailed
)

// classifyReadyStatus maps a readyStatus string to what a wait should do about it.
//
// The vocabulary is the backend's, computed per workload type (see the backend's
// routers/agents.py and routers/tools.py):
//
//	Ready         every workload path        ready
//	Not Ready     every workload path        pending
//	Progressing   statefulset, job, tool     pending
//	Pending       sandbox, no Ready cond.    pending
//	Building      source build in flight     pending
//	Unknown       unrecognized workload      pending
//	Failed        job or rollout failure     failed
//	Build Failed  source build failed        failed
//
// plus "Running", which is what an in-process cortex reports for every instance
// it knows about — the record's existence is the claim that it is running (see
// serve.instanceStatus). Treating it as ready is not a special case for that
// backend but an entry in the same readiness vocabulary; without it, every wait
// against a cortex context would run to its timeout.
//
// Failed and Build Failed are distinguished from pending so a wait can stop
// immediately. Polling a failed build until the timeout would both delay the
// answer and misreport its cause, sending the reader to check timeouts when the
// fix is in their source.
//
// An unrecognized value — including the empty string a server sends before it
// has computed one — is pending, not failed. A status this build predates must
// not turn every wait into a failure; treating it as pending degrades to a
// bounded timeout, which reports uncertainty instead of inventing a verdict.
func classifyReadyStatus(status string) readyState {
	switch strings.TrimSpace(status) {
	case "Ready", "Running":
		return stateReady
	case "Failed", "Build Failed":
		return stateFailed
	default:
		return statePending
	}
}

// waitForReady polls fetch until the resource reports a ready status, and
// returns nil once it does.
//
// kind names the resource type ("agent", "tool") for messages. A timeout of zero
// waits indefinitely, bounded only by the command's context.
//
// The loop probes before it sleeps, so an already-ready resource returns without
// delay — the common case for a wait re-run in a script. The deadline is checked
// after each probe rather than before, so the last sleep is always followed by
// one more attempt instead of expiring in the gap.
//
// A failed probe is not fatal, with one exception: a 404 means the server denies
// the resource exists, which no amount of waiting changes, so it is returned
// immediately rather than spending the timeout on a mistyped name. Other errors
// (5xx, a refused connection) describe the path to the server rather than the
// resource, so they are retried and the timeout is left to bound them.
func waitForReady(
	cmd *cobra.Command,
	fetch readyFetcher,
	kind, namespace, name string,
	timeout time.Duration,
) error {
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()

	var deadline time.Time
	if timeout > 0 {
		deadline = timeNow().Add(timeout)
	}

	// lastStatus is reported on timeout: "stuck at Building" and "stuck at Not
	// Ready" call for different next steps, and the difference is invisible once
	// the command has exited.
	lastStatus := ""
	announced := false

	for {
		detail, err := fetch(ctx)
		switch {
		case err == nil:
			lastStatus = detail.ReadyStatus
			switch classifyReadyStatus(lastStatus) {
			case stateReady:
				return nil
			case stateFailed:
				return waitTerminalError(kind, name, lastStatus)
			}
		case isNotFound(err):
			return err
		case verbose:
			fmt.Fprintf(errOut, "%s %q: %v\n", kind, name, err)
		}

		if !deadline.IsZero() && timeNow().After(deadline) {
			return waitTimeoutError(kind, name, lastStatus, timeout)
		}

		// Say something once if this is taking long enough to notice, so a pause
		// here does not look like a hang. Once, and on stderr, so a wait inside a
		// pipeline neither spams the log nor pollutes stdout.
		if !announced && verbose {
			fmt.Fprintf(errOut, "waiting for %s %q in namespace %q to become ready (currently %q)\n",
				kind, name, namespace, orDefault(lastStatus, "unknown"))
			announced = true
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeAfter(waitPollInterval):
		}
	}
}

// waitTerminalError reports a status that readiness will never follow.
func waitTerminalError(kind, name, status string) error {
	return fmt.Errorf("%s %q reported status %q and will not become ready.\n"+
		"Something the deployment depends on failed, so it is not serving.\n"+
		"Check with `rossoctl %ss get %s`", kind, name, status, kind, name)
}

// waitTimeoutError reports that the timeout elapsed, naming the last status seen
// so the reader knows whether it was stuck or merely slow.
func waitTimeoutError(kind, name, lastStatus string, timeout time.Duration) error {
	return fmt.Errorf("%s %q did not become ready within %s (last status %q).\n"+
		"It may still be starting, or it may be stuck.\n"+
		"Check with `rossoctl %ss get %s`, or allow longer with --timeout",
		kind, name, timeout, orDefault(lastStatus, "unknown"), kind, name)
}

// isNotFound reports whether err is the server answering 404.
func isNotFound(err error) bool {
	var statusErr *apiclient.StatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound
}
