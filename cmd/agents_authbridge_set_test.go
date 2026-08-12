package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

// policyFileBody is the document written by --policy-file. It is YAML, not JSON,
// and carries a comment: both are things a client that parsed and re-encoded the
// file would destroy, so the byte-for-byte assertions below are meaningful.
const policyFileBody = `# inbound policy for orders
mode: proxy-sidecar
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
      - name: opa
        config:
          policy_url: http://opa:8181/v1/data/authz
`

// writePolicyFile writes policyFileBody to a temp file and returns its path.
func writePolicyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the policy file: %v", err)
	}
	return path
}

// fakeIdentityServer records what `agents authbridge set` sent and controls what
// each successive GET answers, which is what the --wait tests need: the poll
// stops on the first response that differs from the baseline, so a test drives it
// by choosing the sequence of GET bodies.
type fakeIdentityServer struct {
	*httptest.Server

	mu sync.Mutex

	// getBodies are served to successive GETs. The last entry is repeated once
	// exhausted, so a test that wants "never changes" supplies just one.
	getBodies []string
	getCount  int

	// putBodies are the bodies of every PUT received, in order, and putCT their
	// Content-Type headers.
	putBodies []string
	putCT     []string

	// getStatus, when non-zero, is the status served for every GET.
	getStatus int
	// putStatus, when non-zero, is the status served for every PUT.
	putStatus int
}

// newFakeIdentityServer serves the identity-config path for agent orders in
// namespace team1, plus the namespace list set-context validates against.
func newFakeIdentityServer(t *testing.T, getBodies ...string) *fakeIdentityServer {
	t.Helper()
	f := &fakeIdentityServer{getBodies: getBodies}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))
			return
		}
		if r.URL.Path != "/api/v1/agents/team1/orders/identity-config" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			f.putBodies = append(f.putBodies, string(body))
			f.putCT = append(f.putCT, r.Header.Get("Content-Type"))
			if f.putStatus != 0 {
				w.WriteHeader(f.putStatus)
				_, _ = w.Write([]byte(`{"detail":"nope"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case http.MethodGet:
			if f.getStatus != 0 {
				w.WriteHeader(f.getStatus)
				_, _ = w.Write([]byte(`{"detail":"not found"}`))
				return
			}
			body := `{"AuthBridge":false}`
			if len(f.getBodies) > 0 {
				i := f.getCount
				if i >= len(f.getBodies) {
					i = len(f.getBodies) - 1
				}
				body = f.getBodies[i]
			}
			f.getCount++
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected method %q", r.Method)
		}
	}))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeIdentityServer) puts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.putBodies...)
}

func (f *fakeIdentityServer) gets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCount
}

func (f *fakeIdentityServer) contentTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.putCT...)
}

// fastPolling shortens the wait so the polling tests do not sleep in real time,
// and restores the production values afterwards.
func fastPolling(t *testing.T, interval, timeout time.Duration) {
	t.Helper()
	oldInterval, oldTimeout := authbridgeWaitInterval, authbridgeWaitTimeout
	authbridgeWaitInterval, authbridgeWaitTimeout = interval, timeout
	t.Cleanup(func() {
		authbridgeWaitInterval, authbridgeWaitTimeout = oldInterval, oldTimeout
	})
}

// TestAgentsAuthbridgeSetSendsFileVerbatim is the central assertion: the bytes
// on the wire are the bytes on disk, as text/plain. The server writes them into a
// ConfigMap, so any reformatting here would silently alter the user's policy.
func TestAgentsAuthbridgeSetSendsFileVerbatim(t *testing.T) {
	isolateHome(t)
	srv := newFakeIdentityServer(t)
	setupAgentGetContext(t, srv.Server)
	policy := writePolicyFile(t, policyFileBody)

	out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy)
	if err != nil {
		t.Fatalf("agents authbridge set: %v\noutput:\n%s", err, out)
	}

	puts := srv.puts()
	if len(puts) != 1 {
		t.Fatalf("PUT count = %d, want 1", len(puts))
	}
	if puts[0] != policyFileBody {
		t.Errorf("PUT body was not the file verbatim.\n got: %q\nwant: %q", puts[0], policyFileBody)
	}
	if cts := srv.contentTypes(); len(cts) != 1 || cts[0] != "text/plain" {
		t.Errorf("Content-Type = %v, want [text/plain]", cts)
	}
	if !strings.Contains(out, "orders") {
		t.Errorf("output should name the agent: %q", out)
	}
}

// TestAgentsAuthbridgeSetWithoutWaitDoesNotGet pins the decision that a plain
// `set` is one request. Reading first would make the command fail for an agent
// whose sidecar is unreachable, even though the write itself would have worked.
func TestAgentsAuthbridgeSetWithoutWaitDoesNotGet(t *testing.T) {
	isolateHome(t)
	srv := newFakeIdentityServer(t)
	setupAgentGetContext(t, srv.Server)
	policy := writePolicyFile(t, policyFileBody)

	if out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy); err != nil {
		t.Fatalf("agents authbridge set: %v\noutput:\n%s", err, out)
	}

	if got := srv.gets(); got != 0 {
		t.Errorf("GET count = %d, want 0 without --wait", got)
	}
}

// TestAgentsAuthbridgeSetWaitStopsOnChange verifies the poll returns once the
// served configuration differs from the pre-PUT baseline, and that it did wait
// rather than accepting the first answer.
func TestAgentsAuthbridgeSetWaitStopsOnChange(t *testing.T) {
	isolateHome(t)
	fastPolling(t, time.Millisecond, 10*time.Second)

	const before = `{"AuthBridge":true,"mode":"transparent","pipeline":{}}`
	const after = `{"AuthBridge":true,"mode":"proxy-sidecar","pipeline":{}}`
	// Baseline, then two unchanged answers (a rollout in progress), then the new
	// configuration.
	srv := newFakeIdentityServer(t, before, before, before, after)
	setupAgentGetContext(t, srv.Server)
	policy := writePolicyFile(t, policyFileBody)

	out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy, "--wait")
	if err != nil {
		t.Fatalf("agents authbridge set --wait: %v\noutput:\n%s", err, out)
	}

	// 1 baseline + 3 polls: the poll must not have stopped on the unchanged ones.
	if got := srv.gets(); got != 4 {
		t.Errorf("GET count = %d, want 4 (baseline plus three polls)", got)
	}
	if len(srv.puts()) != 1 {
		t.Errorf("PUT count = %d, want 1", len(srv.puts()))
	}
	if !strings.Contains(out, "reported the new configuration") {
		t.Errorf("output should report the change was seen: %q", out)
	}
}

// TestAgentsAuthbridgeSetWaitReadsBaselineBeforePut verifies the ordering the
// design depends on: the baseline must be read before the write, or it would
// already reflect the new configuration and the poll could never see a change.
func TestAgentsAuthbridgeSetWaitReadsBaselineBeforePut(t *testing.T) {
	isolateHome(t)
	// A short timeout deliberately: this test's server answers immediately, so a
	// correct implementation never approaches it, while a regression that reads
	// the baseline after the PUT can never see a change and would otherwise hang
	// here for the full budget instead of failing.
	fastPolling(t, time.Millisecond, 50*time.Millisecond)

	var order []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
			return
		}
		mu.Lock()
		order = append(order, r.Method)
		n := len(order)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		// The first GET is the baseline; later ones differ from it.
		if n == 1 {
			_, _ = w.Write([]byte(`{"AuthBridge":true,"mode":"transparent"}`))
			return
		}
		_, _ = w.Write([]byte(`{"AuthBridge":true,"mode":"proxy-sidecar"}`))
	}))
	t.Cleanup(srv.Close)
	setupAgentGetContext(t, srv)
	policy := writePolicyFile(t, policyFileBody)

	if out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy, "--wait"); err != nil {
		t.Fatalf("agents authbridge set --wait: %v\noutput:\n%s", err, out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != http.MethodGet || order[1] != http.MethodPut {
		t.Errorf("request order = %v, want GET then PUT", order)
	}
}

// TestAgentsAuthbridgeSetWaitTimesOutWhenUnchanged pins the accepted cost of
// using "changed" as the success signal: re-applying the configuration that is
// already live cannot be confirmed, so it fails rather than reporting a success
// the command cannot actually observe.
func TestAgentsAuthbridgeSetWaitTimesOutWhenUnchanged(t *testing.T) {
	isolateHome(t)
	fastPolling(t, time.Millisecond, 20*time.Millisecond)

	const same = `{"AuthBridge":true,"mode":"proxy-sidecar"}`
	srv := newFakeIdentityServer(t, same)
	setupAgentGetContext(t, srv.Server)
	policy := writePolicyFile(t, policyFileBody)

	out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy, "--wait")
	if err == nil {
		t.Fatalf("a wait that never sees a change should fail\noutput:\n%s", out)
	}
	// The write did happen, and the message must say so: the user needs to know
	// the policy is stored even though the wait failed.
	if len(srv.puts()) != 1 {
		t.Errorf("PUT count = %d, want 1; the write should still have happened", len(srv.puts()))
	}
	for _, want := range []string{"no change", "already"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// TestAgentsAuthbridgeSetWaitAbortsWhenBaselineFails verifies a failed baseline
// read stops the command before writing. A 404 here means the agent name is
// wrong, and writing anyway would leave a ConfigMap nobody asked for.
func TestAgentsAuthbridgeSetWaitAbortsWhenBaselineFails(t *testing.T) {
	isolateHome(t)
	// Short, for the reason given in TestAgentsAuthbridgeSetWaitReadsBaselineBeforePut:
	// an implementation that writes anyway would poll a never-changing endpoint.
	fastPolling(t, time.Millisecond, 50*time.Millisecond)

	srv := newFakeIdentityServer(t)
	srv.getStatus = http.StatusNotFound
	setupAgentGetContext(t, srv.Server)
	policy := writePolicyFile(t, policyFileBody)

	out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy, "--wait")
	if err == nil {
		t.Fatalf("a failing baseline read should fail the command\noutput:\n%s", out)
	}
	if puts := srv.puts(); len(puts) != 0 {
		t.Errorf("PUT count = %d, want 0: nothing should be written when the baseline read fails", len(puts))
	}
}

// TestAgentsAuthbridgeSetWaitAcceptsNoSidecarBaseline verifies the sidecar-less
// answer is a usable baseline rather than an error. {"AuthBridge": false} is an
// HTTP 200 the server returns when no pod answered, and it is exactly the state a
// first-time `set` starts from — so aborting on it would break the main case.
func TestAgentsAuthbridgeSetWaitAcceptsNoSidecarBaseline(t *testing.T) {
	isolateHome(t)
	fastPolling(t, time.Millisecond, 10*time.Second)

	srv := newFakeIdentityServer(t,
		`{"AuthBridge":false}`,
		`{"AuthBridge":false}`,
		`{"AuthBridge":true,"mode":"proxy-sidecar"}`,
	)
	setupAgentGetContext(t, srv.Server)
	policy := writePolicyFile(t, policyFileBody)

	out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy, "--wait")
	if err != nil {
		t.Fatalf("a no-sidecar baseline should be accepted: %v\noutput:\n%s", err, out)
	}
	if len(srv.puts()) != 1 {
		t.Errorf("PUT count = %d, want 1", len(srv.puts()))
	}
}

// TestAgentsAuthbridgeSetWaitIgnoresAuthBridgeFlag verifies the injected
// AuthBridge key alone is not treated as a change. The server sets it from pod
// reachability, so a pod bouncing during a rollout flips it while the
// configuration is still the old one — which would otherwise report success
// before the new policy was ever loaded.
func TestAgentsAuthbridgeSetWaitIgnoresAuthBridgeFlag(t *testing.T) {
	isolateHome(t)
	fastPolling(t, time.Millisecond, 30*time.Millisecond)

	srv := newFakeIdentityServer(t,
		`{"AuthBridge":true,"mode":"transparent"}`,
		// Same configuration, sidecar briefly unreachable, then back.
		`{"AuthBridge":false,"mode":"transparent"}`,
		`{"AuthBridge":true,"mode":"transparent"}`,
	)
	setupAgentGetContext(t, srv.Server)
	policy := writePolicyFile(t, policyFileBody)

	out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy, "--wait")
	if err == nil {
		t.Fatalf("a flipped AuthBridge flag alone is not a configuration change\noutput:\n%s", out)
	}
}

// TestAgentsAuthbridgeSetWaitToleratesPollErrors verifies a failed GET mid-poll
// does not end the wait. The endpoint reads from the agent's own pods, which are
// what a configuration change restarts, so a transient error is expected there.
func TestAgentsAuthbridgeSetWaitToleratesPollErrors(t *testing.T) {
	isolateHome(t)
	fastPolling(t, time.Millisecond, 10*time.Second)

	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		mu.Lock()
		n++
		i := n
		mu.Unlock()

		switch i {
		case 1:
			_, _ = w.Write([]byte(`{"AuthBridge":true,"mode":"transparent"}`))
		case 2, 3:
			// Pods restarting: the endpoint reports itself unreachable.
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"detail":"unreachable"}`))
		default:
			_, _ = w.Write([]byte(`{"AuthBridge":true,"mode":"proxy-sidecar"}`))
		}
	}))
	t.Cleanup(srv.Close)
	setupAgentGetContext(t, srv)
	policy := writePolicyFile(t, policyFileBody)

	if out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy, "--wait"); err != nil {
		t.Fatalf("transient poll errors should not end the wait: %v\noutput:\n%s", err, out)
	}
}

// TestAgentsAuthbridgeSetRequiresPolicyFile verifies the flag is mandatory, so a
// bare `set` cannot write an empty configuration over a working one.
func TestAgentsAuthbridgeSetRequiresPolicyFile(t *testing.T) {
	isolateHome(t)
	srv := newFakeIdentityServer(t)
	setupAgentGetContext(t, srv.Server)

	out, err := execute(t, "agents", "authbridge", "set", "orders")
	if err == nil {
		t.Fatalf("set without --policy-file should fail\noutput:\n%s", out)
	}
	if !strings.Contains(err.Error(), "policy-file") {
		t.Errorf("error should name the missing flag: %v", err)
	}
	if len(srv.puts()) != 0 {
		t.Errorf("PUT count = %d, want 0", len(srv.puts()))
	}
}

// TestAgentsAuthbridgeSetMissingFileDoesNotWrite verifies an unreadable
// --policy-file is reported before any request, so a mistyped path cannot leave a
// partial operation behind.
func TestAgentsAuthbridgeSetMissingFileDoesNotWrite(t *testing.T) {
	isolateHome(t)
	srv := newFakeIdentityServer(t)
	setupAgentGetContext(t, srv.Server)

	missing := filepath.Join(t.TempDir(), "nope.yaml")
	out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", missing)
	if err == nil {
		t.Fatalf("a missing policy file should fail\noutput:\n%s", out)
	}
	if !strings.Contains(err.Error(), "policy-file") {
		t.Errorf("error should name the flag: %v", err)
	}
	if len(srv.puts()) != 0 || srv.gets() != 0 {
		t.Errorf("no request should be made: %d PUTs, %d GETs", len(srv.puts()), srv.gets())
	}
}

// TestAgentsAuthbridgeSetReportsPutFailure verifies a rejected write fails the
// command rather than reporting success.
func TestAgentsAuthbridgeSetReportsPutFailure(t *testing.T) {
	isolateHome(t)
	srv := newFakeIdentityServer(t)
	srv.putStatus = http.StatusForbidden
	setupAgentGetContext(t, srv.Server)
	policy := writePolicyFile(t, policyFileBody)

	out, err := execute(t, "agents", "authbridge", "set", "orders", "--policy-file", policy)
	if err == nil {
		t.Fatalf("a rejected PUT should fail the command\noutput:\n%s", out)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should report the status: %v", err)
	}
}

// TestAgentsAuthbridgeSetUsesNamespaceFlag verifies `agents --namespace` reaches
// the PUT path, not just the GET commands.
func TestAgentsAuthbridgeSetUsesNamespaceFlag(t *testing.T) {
	isolateHome(t)

	var paths []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/namespaces" {
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))
			return
		}
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	setupAgentGetContext(t, srv)
	policy := writePolicyFile(t, policyFileBody)

	if out, err := execute(t, "agents", "--namespace", "team2", "authbridge", "set", "orders",
		"--policy-file", policy); err != nil {
		t.Fatalf("agents authbridge set: %v\noutput:\n%s", err, out)
	}

	mu.Lock()
	defer mu.Unlock()
	want := "PUT /api/v1/agents/team2/orders/identity-config"
	if len(paths) != 1 || paths[0] != want {
		t.Errorf("requests = %v, want [%s]", paths, want)
	}
}

// TestAgentsAuthbridgeSetTakesExactlyOneName verifies the agent name is required
// and singular, so a stray argument is reported rather than ignored.
func TestAgentsAuthbridgeSetTakesExactlyOneName(t *testing.T) {
	isolateHome(t)
	srv := newFakeIdentityServer(t)
	setupAgentGetContext(t, srv.Server)
	policy := writePolicyFile(t, policyFileBody)

	for _, args := range [][]string{
		{"agents", "authbridge", "set", "--policy-file", policy},
		{"agents", "authbridge", "set", "orders", "extra", "--policy-file", policy},
	} {
		out, err := execute(t, args...)
		if err == nil {
			t.Errorf("%v should fail\noutput:\n%s", args, out)
		}
	}
	if len(srv.puts()) != 0 {
		t.Errorf("PUT count = %d, want 0", len(srv.puts()))
	}
}

// TestIdentityConfigChangedSemantics exercises the comparison directly, over the
// shapes the HTTP tests cannot easily produce: key order, numeric formatting, and
// unknown top-level blocks the config type round-trips as raw JSON.
func TestIdentityConfigChangedSemantics(t *testing.T) {
	parse := func(t *testing.T, body string) *apiclient.AgentIdentityConfig {
		t.Helper()
		var cfg apiclient.AgentIdentityConfig
		if err := json.Unmarshal([]byte(body), &cfg); err != nil {
			t.Fatalf("decoding %s: %v", body, err)
		}
		return &cfg
	}

	tests := []struct {
		name        string
		before      string
		after       string
		wantChanged bool
	}{{
		name:        "identical",
		before:      `{"mode":"proxy-sidecar","listener":{"a":1}}`,
		after:       `{"mode":"proxy-sidecar","listener":{"a":1}}`,
		wantChanged: false,
	}, {
		name: "key order only",
		// Reordering keys is not a change: the server may serialize either way.
		before:      `{"mode":"proxy-sidecar","listener":{"a":1,"b":2}}`,
		after:       `{"listener":{"b":2,"a":1},"mode":"proxy-sidecar"}`,
		wantChanged: false,
	}, {
		name:        "mode differs",
		before:      `{"mode":"transparent"}`,
		after:       `{"mode":"proxy-sidecar"}`,
		wantChanged: true,
	}, {
		name: "unknown block differs",
		// A block this client does not model must still count: most of a real
		// configuration is blocks it knows nothing about.
		before:      `{"mode":"proxy-sidecar","listener":{"reverse_proxy_addr":":8080"}}`,
		after:       `{"mode":"proxy-sidecar","listener":{"reverse_proxy_addr":":9090"}}`,
		wantChanged: true,
	}, {
		name:        "AuthBridge flag only",
		before:      `{"AuthBridge":false,"mode":"proxy-sidecar"}`,
		after:       `{"AuthBridge":true,"mode":"proxy-sidecar"}`,
		wantChanged: false,
	}, {
		name:        "no sidecar to real config",
		before:      `{"AuthBridge":false}`,
		after:       `{"AuthBridge":true,"mode":"proxy-sidecar"}`,
		wantChanged: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := identityConfigChanged(parse(t, tc.before), parse(t, tc.after))
			if got != tc.wantChanged {
				t.Errorf("identityConfigChanged = %v, want %v", got, tc.wantChanged)
			}
		})
	}
}
