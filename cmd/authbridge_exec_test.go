package cmd

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
)

// pipelineOnlyConfig is a valid authbridge config that starts no forward proxy:
// the outbound pipeline is built and started, but no egress is intercepted, so no
// proxy variables reach the child. Used by the many tests that only care about
// argument handling and exit codes.
//
// listener.roles is set to reverse (with the backend the validator requires) so
// the forward role is inactive.
//
// Both listeners are bound to per-test free ports rather than disabled, because
// an empty address cannot express "off": config.ApplyPreset cannot tell empty
// from unset and substitutes the preset default (":9094" for the session API,
// ":8080" for the reverse proxy). Left empty, every test using this fixture would
// bind those two real, wildcard ports and collide with each other.
//
// The reverse address matters even though these tests send it no traffic: the
// reverse role is active, so a listener really does open. The backend is a
// deliberately dead port — nothing dials through it, and it exists only because
// the validator requires the field.
func pipelineOnlyConfig(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [reverse]
  reverse_proxy_addr: "127.0.0.1:%d"
  reverse_proxy_backend: "http://127.0.0.1:1"
  session_api_addr: "127.0.0.1:%d"
pipeline:
  outbound:
    plugins:
      - name: static-inject
        config:
          source: mappings
          key_by: static
          key: demo
          mappings:
            demo: sk-demo-token
`, freePort(t), freePort(t))
}

// forwardConfig is a valid forward-role (outbound) host config: it starts a
// forward proxy on forwardAddr and the session API on sessionAPIAddr. A
// forward-only host needs no reverse_proxy_backend.
//
// Pass an empty sessionAPIAddr to get a per-test free loopback port. It cannot
// mean "off": see pipelineOnlyConfig for why an empty address becomes ":9094".
func forwardConfig(t *testing.T, forwardAddr, sessionAPIAddr string) string {
	t.Helper()
	if sessionAPIAddr == "" {
		sessionAPIAddr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	}
	return fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: %q
  session_api_addr: %q
pipeline:
  outbound:
    plugins:
      - name: inference-parser
`, forwardAddr, sessionAPIAddr)
}

// forwardAddr returns a free loopback address for a test's forward proxy.
func forwardAddr(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("127.0.0.1:%d", freePort(t))
}

// reverseConfig is a valid reverse-role (inbound) host config: it starts a
// reverse proxy on reverseAddr forwarding to backend, and the session API on a
// per-test free port. A reverse-only host needs no forward_proxy_addr, and
// injects no proxy variables into the child.
//
// The inbound pipeline uses inference-parser rather than jwt-validation: it
// observes and records without rejecting, so a test can prove traffic reached
// the backend without standing up a Keycloak to mint a token.
func reverseConfig(t *testing.T, reverseAddr, backend string) string {
	t.Helper()
	return fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [reverse]
  reverse_proxy_addr: %q
  reverse_proxy_backend: %q
  session_api_addr: "127.0.0.1:%d"
pipeline:
  inbound:
    plugins:
      - name: inference-parser
`, reverseAddr, backend, freePort(t))
}

// writeConfig writes a --config document to a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authbridge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// freePort returns a port with nothing listening on it, so a test can bind the
// session API without colliding with a parallel test or a real service.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// execExitCode runs `authbridge exec` and returns the exit code it reports. A nil
// error means 0; an *exitCodeError carries the child's status. Any other error
// fails the test, since these cases are expected to run the command.
func execExitCode(t *testing.T, args ...string) (string, int) {
	t.Helper()
	out, err := execute(t, args...)
	if err == nil {
		return out, 0
	}
	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		return out, exitErr.code
	}
	t.Fatalf("authbridge exec %v: unexpected error: %v\n%s", args, err, out)
	return out, -1
}

// TestAuthbridgeExecCommandPath verifies where exec lives: under the visible
// "authbridge" group, and no longer under "cortex".
func TestAuthbridgeExecCommandPath(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"authbridge", "exec"})
	if err != nil {
		t.Fatalf("authbridge exec not found: %v", err)
	}
	if c.Name() != "exec" || c.Parent() == nil || c.Parent().Name() != "authbridge" {
		t.Errorf("exec resolved to %q under %v, want exec under authbridge", c.Name(), c.Parent())
	}
	if c.Hidden {
		t.Error("authbridge exec should not be hidden")
	}
	if c.Parent().Hidden {
		t.Error("the authbridge group should not be hidden")
	}

	// `cortex exec` must no longer resolve. Cobra's Find falls back to the
	// nearest matching parent, so the check is that it did not land on an "exec"
	// command — it should stop at the cortex group itself.
	if got, _, _ := rootCmd.Find([]string{"cortex", "exec"}); got != nil && got.Name() == "exec" {
		t.Error("cortex exec still resolves; it should have moved to authbridge exec")
	}
	// The cortex group must not list an exec subcommand either.
	if cortex, _, ferr := rootCmd.Find([]string{"cortex"}); ferr == nil {
		for _, sub := range cortex.Commands() {
			if sub.Name() == "exec" {
				t.Error("the cortex group still has an exec subcommand")
			}
		}
	}
}

// TestAuthbridgeCortexFlagRemoved verifies --cortex is gone from the authbridge
// group. exec is configured by --config and never resolves a context, so the flag
// did nothing; it is now removed rather than deprecated, and passing it is an
// error rather than a silently ignored argument.
func TestAuthbridgeCortexFlagRemoved(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"authbridge"})
	if err != nil {
		t.Fatalf("authbridge not found: %v", err)
	}
	// Lookup covers the group's own flags; the inherited set is what exec
	// actually parses with, and catches the flag reappearing on a parent.
	if f := c.PersistentFlags().Lookup("cortex"); f != nil {
		t.Error("authbridge still has a --cortex persistent flag")
	}
	if f := authbridgeExecCmd.InheritedFlags().Lookup("cortex"); f != nil {
		t.Error("authbridge exec still inherits a --cortex flag")
	}

	// The removal has to be visible at the command line, not just in the flag
	// set: an invocation still passing --cortex must fail rather than run.
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))
	_, err = execute(t, "authbridge", "exec", "--cortex", "depctx",
		"--config", cfg, "--", "true")
	if err == nil {
		t.Fatal("authbridge exec --cortex should be rejected as an unknown flag")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error = %q, want it to report an unknown flag", err)
	}
	// And the rejection must happen before the hosted command runs.
	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		t.Errorf("the command should not have run: %v", err)
	}

	// --cortex must survive where it means something: the removal is scoped to
	// the authbridge group, not to the flag itself.
	cortex, _, err := rootCmd.Find([]string{"cortex"})
	if err != nil {
		t.Fatalf("cortex not found: %v", err)
	}
	if cortex.PersistentFlags().Lookup("cortex") == nil {
		t.Error("the cortex group should still have --cortex")
	}
}

// TestAuthbridgeExecLeavesContextAlone is the regression test for exec's context
// independence. exec is configured entirely by --config, so hosting a command
// behind a pipeline must not create a context, switch the current one, or even
// bring the config file into existence — it previously did all three via
// resolveCortexContext, silently repointing every later rossoctl invocation.
func TestAuthbridgeExecLeavesContextAlone(t *testing.T) {
	t.Run("no config file is created", func(t *testing.T) {
		path := isolateHome(t)
		cfg := writeConfig(t, pipelineOnlyConfig(t))

		if _, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "true"); code != 0 {
			t.Fatalf("exec failed: exit %d", code)
		}

		// The context config must not be brought into existence at all.
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("exec created %s; it should not touch the context config (stat err: %v)", path, err)
		}
	})

	t.Run("the current context is preserved", func(t *testing.T) {
		isolateHome(t)

		// Establish a known current context, then confirm exec leaves it as-is.
		if _, err := execute(t, "config", "create-context",
			"--name", "mine", "--server", "http://mine/api/v1/"); err != nil {
			t.Fatalf("create-context: %v", err)
		}
		before := loadTestConfig(t)
		wantCurrent := before.CurrentContext
		wantCount := len(before.Contexts)

		cfg := writeConfig(t, pipelineOnlyConfig(t))
		if _, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "true"); code != 0 {
			t.Fatalf("exec failed: exit %d", code)
		}

		after := loadTestConfig(t)
		if after.CurrentContext != wantCurrent {
			t.Errorf("current context = %q, want %q; exec must not switch contexts",
				after.CurrentContext, wantCurrent)
		}
		if len(after.Contexts) != wantCount {
			t.Errorf("context count = %d, want %d; exec must not add a context",
				len(after.Contexts), wantCount)
		}
	})
}

// TestAuthbridgeSessionServerOverride covers --sessionServer: unset leaves the
// config alone, an explicit address overrides it and forces session tracking on,
// and an explicit empty value turns session tracking off.
func TestAuthbridgeSessionServerOverride(t *testing.T) {
	tests := []struct {
		name string
		// flag is passed only when set is true, so the "unset" case exercises the
		// default rather than an explicit value.
		set          bool
		flag         string
		sessionBlock string
		wantAPI      bool
		// wantAddr is the address the API must be announced on; empty means "the
		// config's own address".
		wantAddr string
	}{
		{name: "unset uses the config address", set: false, wantAPI: true},
		{name: "unset respects session.enabled=false", set: false,
			sessionBlock: "session:\n  enabled: false\n", wantAPI: false},
		{name: "explicit empty disables sessions", set: true, flag: "", wantAPI: false},
		{name: "explicit empty overrides an enabled config", set: true, flag: "",
			sessionBlock: "session:\n  enabled: true\n", wantAPI: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)

			apiAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
			cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: %q
  session_api_addr: %q
%spipeline:
  outbound:
    plugins:
      - name: inference-parser
`, forwardAddr(t), apiAddr, tc.sessionBlock))

			args := []string{"authbridge", "exec", "--config", cfg,
				"--logfile", filepath.Join(t.TempDir(), "authbridge.log")}
			if tc.set {
				args = append(args, "--sessionServer", tc.flag)
			}
			args = append(args, "--", "true")

			_, stderr, err := executeSplit(t, args...)
			if err != nil {
				t.Fatalf("authbridge exec: %v\n%s", err, stderr)
			}

			gotAPI := strings.Contains(stderr, "session API listening on ")
			if gotAPI != tc.wantAPI {
				t.Fatalf("session API started = %v, want %v:\n%s", gotAPI, tc.wantAPI, stderr)
			}
			if tc.wantAPI {
				want := tc.wantAddr
				if want == "" {
					want = apiAddr
				}
				if got := announcedSessionAddr(t, stderr); got != want {
					t.Errorf("session API on %q, want %q", got, want)
				}
			}
		})
	}
}

// TestAuthbridgeSessionServerForcesSessionsOn verifies that naming an address
// explicitly turns session tracking on even when the config disabled it — asking
// for an address should not be silently defeated by session.enabled=false — and
// that the resulting endpoint really serves.
func TestAuthbridgeSessionServerForcesSessionsOn(t *testing.T) {
	isolateHome(t)

	apiAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: %q
  session_api_addr: "127.0.0.1:1"
session:
  enabled: false
pipeline:
  outbound:
    plugins:
      - name: inference-parser
`, forwardAddr(t)))

	// The child proves the endpoint is live, not merely announced.
	script := fmt.Sprintf(`curl -sf -m 5 http://%s/healthz >/dev/null || exit 31`, apiAddr)
	stdout, stderr, err := executeSplit(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"),
		"--sessionServer", apiAddr, "--", "sh", "-c", script)
	if err != nil {
		t.Fatalf("authbridge exec: %v\n%s%s", err, stdout, stderr)
	}
	if got := announcedSessionAddr(t, stderr); got != apiAddr {
		t.Errorf("session API on %q, want the --sessionServer address %q", got, apiAddr)
	}
}

// TestAuthbridgeSessionServerDefault verifies the documented default.
func TestAuthbridgeSessionServerDefault(t *testing.T) {
	f := authbridgeExecCmd.Flags().Lookup("sessionServer")
	if f == nil {
		t.Fatal("authbridge exec has no --sessionServer flag")
	}
	if f.DefValue != "localhost:9094" {
		t.Errorf("--sessionServer default = %q, want localhost:9094", f.DefValue)
	}
	if defaultSessionServer != "localhost:9094" {
		t.Errorf("defaultSessionServer = %q, want localhost:9094", defaultSessionServer)
	}
}

// TestCortexGroupHidden verifies the cortex group is hidden from help listings
// while still being invocable by name.
func TestCortexGroupHidden(t *testing.T) {
	c, _, err := rootCmd.Find([]string{"cortex"})
	if err != nil {
		t.Fatalf("cortex not found: %v", err)
	}
	if !c.Hidden {
		t.Error("the cortex group should be hidden")
	}

	// Hidden must not mean removed: a subcommand still runs when named.
	if _, err := execute(t, "cortex", "status"); err != nil {
		t.Errorf("hidden cortex status should still run: %v", err)
	}

	// ...and it must not appear in the root help listing.
	out, err := execute(t, "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "cortex ") {
			t.Errorf("cortex should not be listed in root help: %q", line)
		}
	}
	if !strings.Contains(out, "authbridge") {
		t.Errorf("authbridge should be listed in root help:\n%s", out)
	}
}

// TestCortexExecRequiresConfig verifies that --config is mandatory: without it
// there is no pipeline to host the command, so exec must refuse rather than run
// the command unprotected.
func TestCortexExecRequiresConfig(t *testing.T) {
	isolateHome(t)

	_, err := execute(t, "authbridge", "exec", "--", "true")
	if err == nil {
		t.Fatal("authbridge exec without --config should error")
	}
	if !strings.Contains(err.Error(), "--config is required") {
		t.Errorf("error = %v, want it to say --config is required", err)
	}
	// It must not be reported as a command status: the command never ran.
	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		t.Errorf("missing --config should not yield a command exit code: %v", err)
	}
}

// TestCortexExecReturnsChildExitCode verifies the central promise of the
// command: the status of the child process becomes rossoctl's own, whether zero
// or not.
func TestCortexExecReturnsChildExitCode(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	for _, want := range []int{0, 1, 7, 42, 255} {
		t.Run(strconv.Itoa(want), func(t *testing.T) {
			script := "exit " + strconv.Itoa(want)
			_, got := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c", script)
			if got != want {
				t.Errorf("exit code = %d, want %d", got, want)
			}
		})
	}
}

// TestCortexExecPassesArgsThroughIntact verifies that everything after "--"
// reaches the command unchanged — including strings that look like rossoctl's
// own flags, an empty argument, embedded spaces, and a second "--".
func TestCortexExecPassesArgsThroughIntact(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	want := []string{"--verbose", "-v", "--server", "two words", "", "--flag=va l", "--", "-n"}
	args := append([]string{"authbridge", "exec", "--config", cfg, "--", "printf", "[%s]\n"}, want...)

	out, code := execExitCode(t, args...)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	for _, w := range want {
		if !strings.Contains(out, "["+w+"]") {
			t.Errorf("argument %q did not reach the command intact:\n%s", w, out)
		}
	}
}

// TestCortexExecRequiresDashDelimiter verifies that the command must be
// separated by "--", so a leading flag in it is never mistaken for rossoctl's.
func TestCortexExecRequiresDashDelimiter(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no delimiter", []string{"authbridge", "exec", "--config", cfg}, `separate it with "--"`},
		{"command without delimiter", []string{"authbridge", "exec", "--config", cfg, "true"}, `separate it with "--"`},
		{"delimiter with no command", []string{"authbridge", "exec", "--config", cfg, "--"}, `no command given after "--"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execute(t, tc.args...)
			if err == nil {
				t.Fatalf("%v should have errored", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestCortexExecArgBeforeDashRejected verifies that a positional argument
// before "--" is reported rather than silently ignored.
func TestCortexExecArgBeforeDashRejected(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	_, err := execute(t, "authbridge", "exec", "--config", cfg, "stray", "--", "true")
	if err == nil {
		t.Fatal("an argument before -- should error")
	}
	if !strings.Contains(err.Error(), "stray") {
		t.Errorf("error should name the stray argument: %v", err)
	}
}

// TestCortexExecStartFailureIsNotAnExitCode verifies that a command which never
// ran is an ordinary rossoctl error, not a pass-through status. Conflating the
// two would report "exit 1" for a typo'd binary name.
func TestCortexExecStartFailureIsNotAnExitCode(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	_, err := execute(t, "authbridge", "exec", "--config", cfg, "--", "no-such-binary-xyz-12345")
	if err == nil {
		t.Fatal("a missing command should error")
	}
	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		t.Errorf("a command that never started should not yield an exit code: %v", err)
	}
	if !strings.Contains(err.Error(), "no-such-binary-xyz-12345") {
		t.Errorf("error should name the command: %v", err)
	}
}

// TestCortexExecSessionAPIServesWhileCommandRuns verifies the session API is up
// for the duration of the command — the child itself queries /healthz and the
// warmed plugin catalog at /v1/plugins — and that the listener is gone once exec
// returns.
func TestCortexExecSessionAPIServesWhileCommandRuns(t *testing.T) {
	isolateHome(t)

	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, forwardConfig(t, forwardAddr(t), addr))

	// The child curls both endpoints, so a pass proves the server was live
	// while the command was running, not merely constructed.
	script := fmt.Sprintf(
		`curl -sf -m 5 http://%s/healthz >/dev/null || exit 21;`+
			`curl -sf -m 5 http://%s/v1/plugins | grep -q inference-parser || exit 22`, addr, addr)

	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c", script)
	switch code {
	case 0: // both endpoints answered
	case 21:
		t.Fatalf("session API /healthz did not answer at %s\n%s", addr, out)
	case 22:
		t.Fatalf("session API /v1/plugins did not include the catalog\n%s", out)
	default:
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	// After exec returns the listener must be closed.
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Errorf("session API still listening on %s after exec returned", addr)
	}
}

// TestCortexExecNoSessionAPIWhenAddrEmpty verifies that an empty
// session_api_addr disables the endpoint rather than binding a default port.
func TestCortexExecNoSessionAPIWhenAddrEmpty(t *testing.T) {
	isolateHome(t)

	// Pick a port, leave it unbound, and assert nothing binds it.
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	if _, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "true"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		_ = c.Close()
		t.Errorf("something is listening on %s though the session API was disabled", addr)
	}
}

// TestCortexExecForwardProxyServesAndInjectsHTTPProxy verifies the forward-role
// behavior end to end: a proxy is bound, HTTP_PROXY points the command at it,
// the command's traffic actually flows through it, and the listener is gone once
// exec returns. Without the TLS bridge, only the HTTP variables are set.
func TestCortexExecForwardProxyServesAndInjectsHTTPProxy(t *testing.T) {
	isolateHome(t)

	// A stand-in origin so the test never reaches the network.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("origin-reached"))
	}))
	defer origin.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, forwardConfig(t, addr, ""))

	// The child reports the injected variables, then fetches through the proxy.
	script := fmt.Sprintf(`
printf 'HTTP_PROXY=%%s\n' "$HTTP_PROXY"
printf 'HTTPS_PROXY=%%s\n' "${HTTPS_PROXY:-<unset>}"
printf 'SSL_CERT_FILE=%%s\n' "${SSL_CERT_FILE:-<unset>}"
curl -sf -m 5 -x "$HTTP_PROXY" %s`, origin.URL)

	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if want := "HTTP_PROXY=http://" + addr; !strings.Contains(out, want) {
		t.Errorf("expected %q in the command's environment:\n%s", want, out)
	}
	// No TLS bridge in this config, so the HTTPS/CA variables must stay unset.
	if !strings.Contains(out, "HTTPS_PROXY=<unset>") {
		t.Errorf("HTTPS_PROXY should be unset without a TLS bridge:\n%s", out)
	}
	if !strings.Contains(out, "SSL_CERT_FILE=<unset>") {
		t.Errorf("CA trust vars should be unset without a TLS bridge:\n%s", out)
	}
	// The proxy actually carried the request.
	if !strings.Contains(out, "origin-reached") {
		t.Errorf("request did not reach the origin through the forward proxy:\n%s", out)
	}

	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Errorf("forward proxy still listening on %s after exec returned", addr)
	}
}

// TestAuthbridgeExecResolvesEphemeralPorts verifies that port 0 — where the kernel
// assigns the port — is resolved to the real bound port. The configured ":0" is
// not dialable, so injecting or announcing it verbatim would hand the command a
// useless proxy address.
func TestAuthbridgeExecResolvesEphemeralPorts(t *testing.T) {
	isolateHome(t)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("origin-reached"))
	}))
	defer origin.Close()

	// Both listeners on port 0.
	cfg := writeConfig(t, forwardConfig(t, "127.0.0.1:0", "127.0.0.1:0"))

	// The child proves the injected proxy address works by fetching through it.
	script := fmt.Sprintf(`
printf 'HTTP_PROXY=%%s\n' "$HTTP_PROXY"
curl -sf -m 5 -x "$HTTP_PROXY" %s`, origin.URL)

	stdout, stderr, err := executeSplit(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"),
		"--", "sh", "-c", script)
	if err != nil {
		t.Fatalf("authbridge exec: %v\n%s%s", err, stdout, stderr)
	}

	// Neither the injected variable nor the announcement may contain ":0".
	if strings.Contains(stdout, ":0\n") || strings.Contains(stdout, "HTTP_PROXY=http://127.0.0.1:0") {
		t.Errorf("HTTP_PROXY was not resolved to a real port:\n%s", stdout)
	}
	if strings.Contains(stderr, "listening on 127.0.0.1:0") {
		t.Errorf("an announced address still says port 0:\n%s", stderr)
	}
	// The resolved proxy actually carried traffic.
	if !strings.Contains(stdout, "origin-reached") {
		t.Errorf("request did not reach the origin through the resolved proxy:\n%s\n%s", stdout, stderr)
	}

	// The announced session API port must be a real, non-zero port.
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "session API listening on ") {
			continue
		}
		addr := line[strings.LastIndex(line, " ")+1:]
		_, port, serr := net.SplitHostPort(addr)
		if serr != nil {
			t.Errorf("could not parse the announced session API address %q: %v", addr, serr)
		} else if port == "0" {
			t.Errorf("the session API was announced on port 0: %q", addr)
		}
	}
}

// TestAuthbridgeExecSessionAPIBindFailureReported verifies that a session API which
// cannot bind is reported synchronously, rather than the command running in a
// host whose endpoint silently never came up.
func TestAuthbridgeExecSessionAPIBindFailureReported(t *testing.T) {
	isolateHome(t)

	// Hold a port so the session API cannot have it.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer busy.Close()

	cfg := writeConfig(t, forwardConfig(t, forwardAddr(t), busy.Addr().String()))
	_, execErr := execute(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"), "--", "true")
	if execErr == nil {
		t.Fatal("a session API that cannot bind should error")
	}
	if !strings.Contains(execErr.Error(), "session API listen") {
		t.Errorf("error should name the failed listen: %v", execErr)
	}
	var exitErr *exitCodeError
	if errors.As(execErr, &exitErr) {
		t.Errorf("a bind failure should not yield a command exit code: %v", execErr)
	}
}

// TestCortexExecNoForwardProxyWithoutForwardRole verifies that a config whose
// active roles exclude "forward" starts no proxy and injects no proxy variables:
// the pipeline is hosted, but nothing is intercepted.
func TestCortexExecNoForwardProxyWithoutForwardRole(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c",
		`printf 'proxy_vars=%s\n' "$(env | grep -ci '^http_proxy=\|^https_proxy=')"`)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "proxy_vars=0") {
		t.Errorf("no proxy variables should be injected without a forward proxy:\n%s", out)
	}
}

// TestCortexExecForwardRoleUsesPresetAddr verifies that a forward-role config
// which omits forward_proxy_addr still gets a proxy: config.ApplyPreset supplies
// the per-mode default (":8081" for proxy-sidecar), and the command is pointed at
// it. The wildcard bind must be injected as a dialable loopback address.
func TestCortexExecForwardRoleUsesPresetAddr(t *testing.T) {
	isolateHome(t)

	// Skip rather than fail if the preset port is already taken locally: the
	// address is fixed by the preset, so this test cannot pick a free one.
	if c, err := net.DialTimeout("tcp", "127.0.0.1:8081", 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Skip("preset forward-proxy port 8081 is already in use on this host")
	}

	// forward_proxy_addr is deliberately omitted — the preset supplies it.
	cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  session_api_addr: %q
pipeline:
  outbound:
    plugins:
      - name: inference-parser
`, fmt.Sprintf("127.0.0.1:%d", freePort(t))))
	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c",
		`printf 'HTTP_PROXY=%s\n' "$HTTP_PROXY"`)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	// ":8081" must be injected as loopback, not as a bare ":8081" the child
	// cannot dial.
	if !strings.Contains(out, "HTTP_PROXY=http://127.0.0.1:8081") {
		t.Errorf("expected the preset address injected as loopback:\n%s", out)
	}
}

// TestCortexExecTLSBridgeInjectsHTTPSAndCATrust verifies that a non-disabled
// tls_bridge mints a CA and that the command is given both HTTPS_PROXY and the
// CA trust variables — without which the command would reject the bridge's leaf
// certificate.
func TestCortexExecTLSBridgeInjectsHTTPSAndCATrust(t *testing.T) {
	isolateHome(t)

	caDir := filepath.Join(t.TempDir(), "ca")
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: %q
  session_api_addr: %q
tls_bridge:
  mode: enabled
  ca_dir: %q
  generate_ca: true
pipeline:
  outbound:
    plugins:
      - name: inference-parser
`, addr, fmt.Sprintf("127.0.0.1:%d", freePort(t)), caDir))

	script := `
printf 'HTTPS_PROXY=%s\n' "$HTTPS_PROXY"
printf 'NODE_EXTRA_CA_CERTS=%s\n' "$NODE_EXTRA_CA_CERTS"
printf 'REQUESTS_CA_BUNDLE=%s\n' "$REQUESTS_CA_BUNDLE"
printf 'SSL_CERT_FILE=%s\n' "$SSL_CERT_FILE"
test -s "$SSL_CERT_FILE" && printf 'ca_readable=yes\n'`

	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if want := "HTTPS_PROXY=http://" + addr; !strings.Contains(out, want) {
		t.Errorf("expected %q with the TLS bridge enabled:\n%s", want, out)
	}
	caCert := filepath.Join(caDir, "ca.crt")
	for _, v := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE"} {
		if want := v + "=" + caCert; !strings.Contains(out, want) {
			t.Errorf("expected %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "ca_readable=yes") {
		t.Errorf("the CA the command was pointed at is not readable:\n%s", out)
	}
	// The CA must have been minted on disk, since generate_ca was set.
	if _, err := os.Stat(caCert); err != nil {
		t.Errorf("generate_ca did not mint %s: %v", caCert, err)
	}
}

// TestCortexExecTLSBridgeDisabledModes verifies that both an absent tls_bridge
// and an explicitly disabled one leave HTTPS untouched: no CA is minted and no
// HTTPS/CA variables are injected.
func TestCortexExecTLSBridgeDisabledModes(t *testing.T) {
	for _, tc := range []struct{ name, block string }{
		{"absent", ""},
		{"disabled", "tls_bridge:\n  mode: disabled\n"},
		{"empty mode", "tls_bridge:\n  mode: \"\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)

			addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
			cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: %q
  session_api_addr: %q
%spipeline:
  outbound:
    plugins:
      - name: inference-parser
`, addr, fmt.Sprintf("127.0.0.1:%d", freePort(t)), tc.block))

			out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c",
				`printf 'HTTP_PROXY=%s HTTPS_PROXY=%s CA=%s\n' "$HTTP_PROXY" "${HTTPS_PROXY:-<unset>}" "${SSL_CERT_FILE:-<unset>}"`)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0\n%s", code, out)
			}
			// HTTP_PROXY is still injected — the proxy runs; only the bridge is off.
			if !strings.Contains(out, "HTTP_PROXY=http://"+addr) {
				t.Errorf("HTTP_PROXY should still be injected:\n%s", out)
			}
			if !strings.Contains(out, "HTTPS_PROXY=<unset>") || !strings.Contains(out, "CA=<unset>") {
				t.Errorf("no HTTPS/CA variables should be injected without a bridge:\n%s", out)
			}
		})
	}
}

// TestCortexExecSessionsDisabled verifies that session.enabled=false turns off
// the store, and with it the session API, while leaving the proxy running.
func TestCortexExecSessionsDisabled(t *testing.T) {
	isolateHome(t)

	proxyAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	apiAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: %q
  session_api_addr: %q
session:
  enabled: false
pipeline:
  outbound:
    plugins:
      - name: inference-parser
`, proxyAddr, apiAddr))

	// The session API must not answer; the proxy must still be up.
	script := fmt.Sprintf(
		`curl -sf -m 3 http://%s/healthz >/dev/null 2>&1 && printf 'api=up\n' || printf 'api=down\n'`+
			`; curl -sf -m 3 -o /dev/null http://127.0.0.1:1 -x http://%s 2>/dev/null; printf 'proxy_dialable=%%s\n' "$?"`,
		apiAddr, proxyAddr)

	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "api=down") {
		t.Errorf("session API should not serve when session.enabled is false:\n%s", out)
	}
}

// TestCortexExecPreservesInheritedProxyVars verifies that a proxy variable the
// operator exported deliberately is not silently rewritten.
func TestCortexExecPreservesInheritedProxyVars(t *testing.T) {
	isolateHome(t)
	t.Setenv("HTTP_PROXY", "http://operator.example:3128")

	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, forwardConfig(t, addr, ""))

	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c",
		`printf 'HTTP_PROXY=%s\n' "$HTTP_PROXY"`)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "HTTP_PROXY=http://operator.example:3128") {
		t.Errorf("an inherited HTTP_PROXY should be preserved:\n%s", out)
	}
}

// TestProxyURL covers turning a listen address into something the command can
// dial: a wildcard bind has to become loopback.
func TestProxyURL(t *testing.T) {
	tests := []struct{ addr, want string }{
		{"127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"localhost:8081", "http://localhost:8081"},
		{":8081", "http://127.0.0.1:8081"},
		{"0.0.0.0:8081", "http://127.0.0.1:8081"},
		{"[::]:8081", "http://127.0.0.1:8081"},
		{"[::1]:8081", "http://[::1]:8081"},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			if got := proxyURL(tc.addr); got != tc.want {
				t.Errorf("proxyURL(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// TestSessionBounds covers the session store's config-driven bounds, including
// the fallbacks for unset and malformed values.
func TestSessionBounds(t *testing.T) {
	tests := []struct {
		name            string
		yaml            string
		ttl             time.Duration
		events, maxSess int
	}{
		{"defaults", "mode: proxy-sidecar\n", defaultSessionTTL, defaultSessionMaxEvents, defaultSessionMaxSessions},
		{
			name: "explicit", yaml: "mode: proxy-sidecar\nsession:\n  ttl: 5m\n  max_events: 7\n  max_sessions: 3\n",
			ttl: 5 * time.Minute, events: 7, maxSess: 3,
		},
		{
			// A malformed duration falls back rather than failing the run.
			name: "bad ttl falls back", yaml: "mode: proxy-sidecar\nsession:\n  ttl: not-a-duration\n",
			ttl: defaultSessionTTL, events: defaultSessionMaxEvents, maxSess: defaultSessionMaxSessions,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, tc.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			ttl, events, maxSess := sessionBounds(cfg, io.Discard)
			if ttl != tc.ttl || events != tc.events || maxSess != tc.maxSess {
				t.Errorf("sessionBounds = (%s, %d, %d), want (%s, %d, %d)",
					ttl, events, maxSess, tc.ttl, tc.events, tc.maxSess)
			}
		})
	}
}

// TestCortexExecLogfile verifies that authbridge's log output is redirected to
// --logfile instead of stderr, and that the path is announced so the operator can
// find it.
func TestCortexExecLogfile(t *testing.T) {
	isolateHome(t)

	logPath := filepath.Join(t.TempDir(), "authbridge.log")
	// A forward proxy logs "HTTP server listening" through slog, giving us a
	// record to look for.
	cfg := writeConfig(t, forwardConfig(t, forwardAddr(t), ""))

	stdout, stderr, err := executeSplit(t, "authbridge", "exec",
		"--config", cfg, "--logfile", logPath, "--", "true")
	if err != nil {
		t.Fatalf("authbridge exec: %v\n%s%s", err, stdout, stderr)
	}

	// The path is announced unconditionally (no --verbose here).
	if !strings.Contains(stderr, logPath) {
		t.Errorf("the logfile path should be printed at startup:\n%s", stderr)
	}

	body, rerr := os.ReadFile(logPath)
	if rerr != nil {
		t.Fatalf("reading logfile: %v", rerr)
	}
	if !strings.Contains(string(body), "forward-proxy") {
		t.Errorf("authbridge log output did not reach %s:\n%s", logPath, body)
	}
	// ...and it must NOT have gone to stderr, which is the point of the flag.
	if strings.Contains(stderr, "HTTP server listening") {
		t.Errorf("authbridge log output leaked to stderr:\n%s", stderr)
	}
}

// TestCortexExecLogfileAppends verifies the logfile is appended to rather than
// truncated, so successive runs do not destroy earlier output.
func TestCortexExecLogfileAppends(t *testing.T) {
	isolateHome(t)

	logPath := filepath.Join(t.TempDir(), "authbridge.log")
	if err := os.WriteFile(logPath, []byte("PRIOR-CONTENT\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg := writeConfig(t, forwardConfig(t, forwardAddr(t), ""))

	if _, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--logfile", logPath, "--", "true"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading logfile: %v", err)
	}
	if !strings.Contains(string(body), "PRIOR-CONTENT") {
		t.Errorf("the logfile was truncated; earlier content is gone:\n%s", body)
	}
	if !strings.Contains(string(body), "forward-proxy") {
		t.Errorf("new output was not appended:\n%s", body)
	}
}

// TestCortexExecLogfileUnwritable verifies that a logfile which cannot be opened
// is reported before the command runs, rather than silently losing the log.
func TestCortexExecLogfileUnwritable(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	bad := filepath.Join(t.TempDir(), "no-such-dir", "authbridge.log")
	_, err := execute(t, "authbridge", "exec", "--config", cfg, "--logfile", bad, "--", "true")
	if err == nil {
		t.Fatal("an unopenable logfile should error")
	}
	if !strings.Contains(err.Error(), "logfile") {
		t.Errorf("error should mention the logfile: %v", err)
	}
	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		t.Errorf("a logfile failure should not yield a command exit code: %v", err)
	}
}

// TestCortexExecLogfileDefault verifies the documented default path.
//
// It asserts the constant rather than the flag's DefValue, because TestMain
// repoints that default into the test's temp HOME so the suite never appends to a
// developer's real /tmp/authbridge.log.
func TestCortexExecLogfileDefault(t *testing.T) {
	if defaultLogfile != "/tmp/authbridge.log" {
		t.Errorf("defaultLogfile = %q, want /tmp/authbridge.log", defaultLogfile)
	}
	// The flag must exist and be wired to the same variable the default sets.
	if f := authbridgeExecCmd.Flags().Lookup("logfile"); f == nil {
		t.Error("authbridge exec has no --logfile flag")
	}
}

// TestCortexExecSessionAPIAlwaysAnnounced verifies the listening line is printed
// without --verbose, and that the UNAUTHENTICATED warning is reserved for
// wildcard binds — a loopback bind is only reachable from this host, so warning
// there would be noise on every run.
func TestCortexExecSessionAPIAlwaysAnnounced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		host     string
		wantWarn bool
	}{
		{"loopback", "127.0.0.1", false},
		{"wildcard v4", "0.0.0.0", true},
		{"empty host", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)

			port := freePort(t)
			apiAddr := fmt.Sprintf("%s:%d", tc.host, port)
			logPath := filepath.Join(t.TempDir(), "authbridge.log")
			cfg := writeConfig(t, forwardConfig(t, forwardAddr(t), apiAddr))

			// No --verbose: the announcement must appear regardless.
			_, stderr, err := executeSplit(t, "authbridge", "exec",
				"--config", cfg, "--logfile", logPath, "--", "true")
			if err != nil {
				t.Fatalf("authbridge exec: %v\n%s", err, stderr)
			}
			// The announced address is the *bound* one, so match on the port and
			// check the host separately: the kernel normalizes a dual-stack
			// wildcard bind ("0.0.0.0:p" or ":p") to "[::]:p".
			announced := announcedSessionAddr(t, stderr)
			gotHost, gotPort, serr := net.SplitHostPort(announced)
			if serr != nil {
				t.Fatalf("unparseable announced address %q: %v", announced, serr)
			}
			if gotPort != strconv.Itoa(port) {
				t.Errorf("announced port = %s, want %d (from %q)", gotPort, port, announced)
			}
			// A wildcard config must stay a wildcard bind, and a loopback config
			// must stay loopback — the normalization must not widen the bind.
			if isWildcardHost(announced) != tc.wantWarn {
				t.Errorf("announced %q wildcard = %v, want %v",
					announced, isWildcardHost(announced), tc.wantWarn)
			}
			if !tc.wantWarn && gotHost != "127.0.0.1" {
				t.Errorf("a loopback config was announced as %q", announced)
			}

			// The warning goes through slog, so it lands in the logfile.
			body, _ := os.ReadFile(logPath)
			gotWarn := strings.Contains(string(body), "UNAUTHENTICATED")
			if gotWarn != tc.wantWarn {
				t.Errorf("UNAUTHENTICATED warning present = %v, want %v (addr %q):\n%s",
					gotWarn, tc.wantWarn, apiAddr, body)
			}
		})
	}
}

// announcedSessionAddr extracts the address from exec's "session API listening
// on <addr>" line, failing the test when there is no such line.
func announcedSessionAddr(t *testing.T, stderr string) string {
	t.Helper()
	const prefix = "session API listening on "
	for _, line := range strings.Split(stderr, "\n") {
		if i := strings.Index(line, prefix); i >= 0 {
			return strings.TrimSpace(line[i+len(prefix):])
		}
	}
	t.Fatalf("no %q line on stderr:\n%s", strings.TrimSpace(prefix), stderr)
	return ""
}

// TestIsWildcardHost covers the bind-exposure test behind the UNAUTHENTICATED
// warning.
func TestIsWildcardHost(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{":9094", true},
		{"0.0.0.0:9094", true},
		{"[::]:9094", true},
		{"[::0]:9094", true},
		{"127.0.0.1:9094", false},
		{"localhost:9094", false},
		{"[::1]:9094", false},
		{"192.168.1.10:9094", false},
		// Not host:port — treated as specific rather than crying wolf.
		{"9094", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			if got := isWildcardHost(tc.addr); got != tc.want {
				t.Errorf("isWildcardHost(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

// TestContextGuruRegisteredByDefault verifies context-guru is compiled into the
// default build, so the context-guru demo configs run without a special tag.
func TestContextGuruRegisteredByDefault(t *testing.T) {
	plugins.WarmCatalog()
	for _, name := range plugins.RegisteredPlugins() {
		if name == "context-guru" {
			return
		}
	}
	t.Errorf("context-guru is not registered in a default build; registered: %v",
		plugins.RegisteredPlugins())
}

// TestCortexExecRemoteConfigFetchedAndRemoved verifies that a URL config is
// fetched to a temp file — visible to the command while it runs, since
// config.Load is path-based — and that the temp file is removed on exit.
func TestCortexExecRemoteConfigFetchedAndRemoved(t *testing.T) {
	isolateHome(t)

	body := pipelineOnlyConfig(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/authbridge.yaml" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	// Count temp configs before, during (from the child), and after.
	pattern := filepath.Join(os.TempDir(), "rossoctl-cortex-exec-*.yaml")
	before, _ := filepath.Glob(pattern)

	// Read stdout alone: exec's own announcements (logfile path, listener
	// addresses) go to stderr and would otherwise be parsed as part of the count.
	// --logfile keeps this test off the real /tmp/authbridge.log.
	script := fmt.Sprintf("ls %s 2>/dev/null | wc -l", pattern)
	stdout, stderr, err := executeSplit(t, "authbridge", "exec",
		"--config", srv.URL+"/authbridge.yaml",
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"),
		"--", "sh", "-c", script)
	if err != nil {
		t.Fatalf("authbridge exec: %v\n%s%s", err, stdout, stderr)
	}

	// The child must have seen one more temp config than existed before.
	during, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		t.Fatalf("could not read the child's count %q: %v", stdout, err)
	}
	if during != len(before)+1 {
		t.Errorf("temp configs visible to the command = %d, want %d (one fetched config)",
			during, len(before)+1)
	}

	after, _ := filepath.Glob(pattern)
	if len(after) != len(before) {
		t.Errorf("temp configs after exec = %d, want %d — the fetched config was not removed:\n%v",
			len(after), len(before), after)
	}
}

// TestCortexExecLocalConfigNotRemoved verifies exec never deletes a config it
// did not create.
func TestCortexExecLocalConfigNotRemoved(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	if _, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "true"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Errorf("local config was removed or damaged: %v", err)
	}
}

// TestCortexExecConfigURLError verifies that a non-2xx config URL is a rossoctl
// error and the command is never run.
func TestCortexExecConfigURLError(t *testing.T) {
	isolateHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := execute(t, "authbridge", "exec", "--config", srv.URL+"/authbridge.yaml", "--", "true")
	if err == nil {
		t.Fatal("a 404 config URL should error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should report the status: %v", err)
	}
}

// TestCortexExecConfigErrors verifies that an unreadable, malformed, or invalid
// config is reported before the command runs.
func TestCortexExecConfigErrors(t *testing.T) {
	isolateHome(t)

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"missing file", filepath.Join(t.TempDir(), "absent.yaml"), "reading config"},
		{"directory", t.TempDir(), "is a directory"},
		{"not yaml", writeConfig(t, "\tthis: [is: not\n"), "loading config"},
		// A structurally valid document that fails authbridge's own validation:
		// the reverse role needs a backend, which this omits.
		{"invalid config", writeConfig(t, "mode: proxy-sidecar\nlistener:\n  roles: [reverse]\n"), "invalid config"},
		// An outbound plugin that does not exist must be reported by the build.
		{"unknown plugin", writeConfig(t,
			"mode: proxy-sidecar\nlistener:\n  roles: [forward]\npipeline:\n  outbound:\n    plugins:\n      - name: no-such-plugin\n"),
			"outbound pipeline"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execute(t, "authbridge", "exec", "--config", tc.ref, "--", "true")
			if err == nil {
				t.Fatalf("config %q should error", tc.ref)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			var exitErr *exitCodeError
			if errors.As(err, &exitErr) {
				t.Errorf("a config failure should not yield a command exit code: %v", err)
			}
		})
	}
}

// TestCortexExecStdoutAndStderrPassThrough verifies both child streams reach
// the caller.
func TestCortexExecStdoutAndStderrPassThrough(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	stdout, stderr, err := executeSplit(t, "authbridge", "exec", "--config", cfg,
		"--", "sh", "-c", "echo on-stdout; echo on-stderr >&2")
	if err != nil {
		t.Fatalf("authbridge exec: %v", err)
	}
	if !strings.Contains(stdout, "on-stdout") {
		t.Errorf("child stdout not passed through: %q", stdout)
	}
	if !strings.Contains(stderr, "on-stderr") {
		t.Errorf("child stderr not passed through: %q", stderr)
	}
}

// TestCortexExecStdinPassThrough verifies the command's stdin is connected.
func TestCortexExecStdinPassThrough(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	rootCmd.SetIn(strings.NewReader("hello-stdin\n"))
	t.Cleanup(func() { rootCmd.SetIn(nil) })

	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "cat")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "hello-stdin") {
		t.Errorf("stdin was not passed through: %q", out)
	}
}

// TestCortexExecContextOverrideMustExist verifies that --context naming an
// unknown context errors before the command runs. exec ignores the context, but
// an explicit --context that does not resolve is a typo worth reporting rather
// than silently accepting.
func TestCortexExecContextOverrideMustExist(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	_, err := execute(t, "authbridge", "exec", "--context", "ghost", "--config", cfg, "--", "true")
	if err == nil {
		t.Fatal("--context ghost should error")
	}
	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		t.Errorf("the command should not have run: %v", err)
	}
}

// TestCortexExecContextOverrideCreatesNothing verifies that validating an
// existing --context stays read-only: naming a real context must not make it
// current or otherwise rewrite the config.
func TestCortexExecContextOverrideCreatesNothing(t *testing.T) {
	isolateHome(t)

	// Two contexts, with "other" current, so a switch to "mine" would show.
	if _, err := execute(t, "config", "create-context", "--name", "mine", "--server", "http://mine/"); err != nil {
		t.Fatalf("create-context mine: %v", err)
	}
	if _, err := execute(t, "config", "create-context", "--name", "other", "--server", "http://other/"); err != nil {
		t.Fatalf("create-context other: %v", err)
	}
	if _, err := execute(t, "config", "use-context", "other"); err != nil {
		t.Fatalf("use-context other: %v", err)
	}

	cfg := writeConfig(t, pipelineOnlyConfig(t))
	if _, code := execExitCode(t, "authbridge", "exec", "--context", "mine",
		"--config", cfg, "--", "true"); code != 0 {
		t.Fatalf("exec with --context mine: exit %d", code)
	}

	if got := loadTestConfig(t).CurrentContext; got != "other" {
		t.Errorf("current context = %q, want %q; --context must not switch the current context", got, "other")
	}
}

// TestSpiffeProviderNeeded covers the need-driven SPIFFE provider decision.
// Building a provider blocks on the SPIRE Workload API, so getting this wrong
// hangs exec on hosts without SPIRE.
func TestSpiffeProviderNeeded(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{"no mtls, no spiffe plugin", "mode: proxy-sidecar\n", false},
		{
			name: "outbound plugin with spiffe identity",
			yaml: "mode: proxy-sidecar\npipeline:\n  outbound:\n    plugins:\n      - name: token-exchange\n        config:\n          identity:\n            type: spiffe\n",
			want: true,
		},
		{
			name: "outbound plugin with another identity type",
			yaml: "mode: proxy-sidecar\npipeline:\n  outbound:\n    plugins:\n      - name: token-exchange\n        config:\n          identity:\n            type: static\n",
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, tc.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := spiffeProviderNeeded(cfg); got != tc.want {
				t.Errorf("spiffeProviderNeeded = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExitStatusSignaled verifies that a command killed by a signal is reported
// as 128+signal rather than letting ExitCode's -1 reach os.Exit (where it would
// silently become 255).
func TestExitStatusSignaled(t *testing.T) {
	// Kill a real process with a signal to get a genuine WaitStatus.
	c := exec.Command("sh", "-c", "kill -TERM $$; sleep 5")
	if err := c.Run(); err == nil {
		t.Fatal("expected the process to be signaled")
	} else {
		var exitErr *exitCodeError
		if !errors.As(exitStatus([]string{"sh"}, err), &exitErr) {
			t.Fatalf("expected an exitCodeError, got %v", err)
		}
		if want := 128 + int(syscall.SIGTERM); exitErr.code != want {
			t.Errorf("exit code = %d, want %d (128+SIGTERM)", exitErr.code, want)
		}
	}
}

// TestIsHTTPURL covers the file-vs-URL discrimination for --config, including
// paths that merely contain a colon.
func TestIsHTTPURL(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{"http://example.com/c.yaml", true},
		{"https://example.com/c.yaml", true},
		// url.Parse lowercases the scheme, and RFC 3986 schemes are
		// case-insensitive, so an uppercase scheme is still a URL.
		{"HTTP://example.com/c.yaml", true},
		{"./authbridge.yaml", false},
		{"authbridge.yaml", false},
		{"/abs/path/authbridge.yaml", false},
		{"file:///abs/authbridge.yaml", false},
		{"http://", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.ref, func(t *testing.T) {
			if got := isHTTPURL(tc.ref); got != tc.want {
				t.Errorf("isHTTPURL(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

// TestAuthbridgeExecReverseProxyForwardsToBackend verifies the reverse proxy
// actually carries inbound traffic: a caller reaching the proxy's address is
// forwarded through the inbound pipeline to reverse_proxy_backend, and the
// listener is gone once exec returns.
//
// This is the end-to-end proof of the feature. The child does the calling, since
// the reverse proxy only runs while the hosted command does.
func TestAuthbridgeExecReverseProxyForwardsToBackend(t *testing.T) {
	isolateHome(t)

	// The backend records the Host header it saw. reverseproxy rewrites it to the
	// backend's own host, which Cloudflare-fronted upstreams require; asserting it
	// here is a cheap guard on that wiring.
	gotHost := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotHost <- r.Host:
		default:
		}
		_, _ = w.Write([]byte("backend-reached"))
	}))
	defer backend.Close()

	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, reverseConfig(t, addr, backend.URL))

	// --retry-connrefused rather than a sleep: startReverseProxy returns only
	// after the listener is bound, so this is belt-and-braces for a loaded box
	// rather than a race, and a fixed sleep would be a flake either way.
	script := fmt.Sprintf(`curl -sf -m 5 --retry 3 --retry-connrefused http://%s/v1/chat/completions`, addr)

	stdout, stderr, err := executeSplit(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"),
		"--", "sh", "-c", script)
	if err != nil {
		t.Fatalf("authbridge exec: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "backend-reached") {
		t.Errorf("request did not reach the backend through the reverse proxy:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// The announcement is how an operator learns where to send callers.
	if want := "reverse proxy listening on " + addr; !strings.Contains(stderr, want) {
		t.Errorf("stderr should announce %q:\n%s", want, stderr)
	}
	if !strings.Contains(stderr, backend.URL) {
		t.Errorf("stderr should name the backend %q:\n%s", backend.URL, stderr)
	}

	// The Host header must name the backend, not the proxy the caller dialed.
	select {
	case h := <-gotHost:
		backendHost := strings.TrimPrefix(backend.URL, "http://")
		if h != backendHost {
			t.Errorf("backend saw Host %q, want the backend's own host %q", h, backendHost)
		}
	default:
		t.Error("backend was never reached, so no Host header was recorded")
	}

	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Errorf("reverse proxy still listening on %s after exec returned", addr)
	}
}

// TestAuthbridgeExecNoReverseProxyWithoutReverseRole verifies that a config whose
// active roles exclude "reverse" opens no reverse listener, even when
// reverse_proxy_addr names one. Roles select which listeners run; an address
// alone must not start one.
func TestAuthbridgeExecNoReverseProxyWithoutReverseRole(t *testing.T) {
	isolateHome(t)

	// A free port named in the config but never expected to be bound.
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: %q
  reverse_proxy_addr: %q
  reverse_proxy_backend: "http://127.0.0.1:1"
  session_api_addr: "127.0.0.1:%d"
pipeline:
  outbound:
    plugins:
      - name: inference-parser
`, forwardAddr(t), addr, freePort(t)))

	// The child probes while the host is up — after exec returns everything is
	// down anyway, so probing then would prove nothing.
	//
	// --noproxy '*' is essential: this is a forward-role host, so the child
	// inherits HTTP_PROXY. Without it curl hands the URL to the forward proxy,
	// which answers, and the probe passes whether or not a reverse listener
	// exists. The question here is specifically whether addr itself is bound.
	script := fmt.Sprintf(`curl -s -m 2 --noproxy '*' http://%s/ >/dev/null 2>&1 || printf 'no-reverse-proxy\n'`, addr)
	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "no-reverse-proxy") {
		t.Errorf("a forward-only config should open no reverse listener on %s:\n%s", addr, out)
	}
}

// TestAuthbridgeExecReverseProxyBindFailureReported verifies that a reverse proxy
// which cannot bind is reported synchronously as a setup failure, rather than the
// command running behind a listener that never came up.
func TestAuthbridgeExecReverseProxyBindFailureReported(t *testing.T) {
	isolateHome(t)

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer busy.Close()

	cfg := writeConfig(t, reverseConfig(t, busy.Addr().String(), "http://127.0.0.1:1"))
	_, execErr := execute(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"), "--", "true")
	if execErr == nil {
		t.Fatal("a reverse proxy that cannot bind should error")
	}
	if !strings.Contains(execErr.Error(), "reverse-proxy listen") {
		t.Errorf("error should name the failed listen: %v", execErr)
	}
	var exitErr *exitCodeError
	if errors.As(execErr, &exitErr) {
		t.Errorf("a bind failure should not yield a command exit code: %v", execErr)
	}
}

// TestAuthbridgeExecReverseProxyResolvesEphemeralPort verifies that a reverse
// address of port 0 is announced as the real bound port. This is why the listener
// is bound here rather than inside runtimeutil.StartReverseProxyServer, which
// keeps it internal: ":0" is not an address anyone can send callers to.
func TestAuthbridgeExecReverseProxyResolvesEphemeralPort(t *testing.T) {
	isolateHome(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("backend-reached"))
	}))
	defer backend.Close()

	cfg := writeConfig(t, reverseConfig(t, "127.0.0.1:0", backend.URL))
	stdout, stderr, err := executeSplit(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"), "--", "true")
	if err != nil {
		t.Fatalf("authbridge exec: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	// A resolved port, not the configured placeholder.
	if !strings.Contains(stderr, "reverse proxy listening on 127.0.0.1:") {
		t.Errorf("stderr should announce the reverse proxy's bound address:\n%s", stderr)
	}
	for _, stream := range []struct{ name, body string }{{"stdout", stdout}, {"stderr", stderr}} {
		if strings.Contains(stream.body, ":0\n") || strings.Contains(stream.body, ":0 ") {
			t.Errorf("%s reports an unresolved port 0:\n%s", stream.name, stream.body)
		}
	}
}

// TestAuthbridgeExecReverseProxyBackendValidation covers validateBackendURL, whose
// job is to reject a backend that url.Parse accepts but nothing can proxy to.
func TestAuthbridgeExecReverseProxyBackendValidation(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		wantErr bool
	}{
		{"http", "http://127.0.0.1:8001", false},
		{"https with path", "https://example.com/api", false},
		{"empty", "", true},
		// Parses cleanly, with "localhost" as the *scheme* — the most common way
		// to get this wrong.
		{"bare host:port", "localhost:8001", true},
		{"scheme only", "http://", true},
		{"not a url", "://x", true},
		{"wrong scheme", "ftp://example.com", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBackendURL(tc.backend)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateBackendURL(%q) = %v, wantErr %v", tc.backend, err, tc.wantErr)
			}
		})
	}
}

// TestAuthbridgeExecReverseProxyBackendValidationWired verifies the check is
// actually reached from exec, which the unit test above cannot show. A bare
// host:port would otherwise bind fine and 502 every request.
func TestAuthbridgeExecReverseProxyBackendValidationWired(t *testing.T) {
	isolateHome(t)

	cfg := writeConfig(t, reverseConfig(t, forwardAddr(t), "localhost:8001"))
	_, execErr := execute(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"), "--", "true")
	if execErr == nil {
		t.Fatal("a backend with no scheme should error")
	}
	if !strings.Contains(execErr.Error(), "reverse_proxy_backend") {
		t.Errorf("error should name the offending field: %v", execErr)
	}
	var exitErr *exitCodeError
	if errors.As(execErr, &exitErr) {
		t.Errorf("a config error should not yield a command exit code: %v", execErr)
	}
}

// TestAuthbridgeExecStrictMTLSRejected verifies that strict mTLS is refused
// rather than silently downgraded. exec has no SPIFFE identity, so it cannot
// reject non-TLS callers as strict demands — serving plaintext to everyone would
// be the opposite of what the config asks for.
func TestAuthbridgeExecStrictMTLSRejected(t *testing.T) {
	isolateHome(t)

	cfg := writeConfig(t, reverseConfig(t, forwardAddr(t), "http://127.0.0.1:1")+
		"mtls:\n  mode: strict\n")
	_, execErr := execute(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"), "--", "true")
	if execErr == nil {
		t.Fatal("strict mtls should be refused without a SPIFFE identity")
	}
	for _, want := range []string{"strict", "SPIFFE"} {
		if !strings.Contains(execErr.Error(), want) {
			t.Errorf("error should mention %q: %v", want, execErr)
		}
	}
	var exitErr *exitCodeError
	if errors.As(execErr, &exitErr) {
		t.Errorf("a config refusal should not yield a command exit code: %v", execErr)
	}
}

// TestAuthbridgeExecPermissiveMTLSRunsWithNotice verifies permissive mTLS starts
// but says it is not enforced. A plaintext-only listener is a subset of what
// permissive accepts, so running is correct — but an operator who wrote an mtls
// block should not have to guess that it is inert here.
func TestAuthbridgeExecPermissiveMTLSRunsWithNotice(t *testing.T) {
	isolateHome(t)

	cfg := writeConfig(t, reverseConfig(t, forwardAddr(t), "http://127.0.0.1:1")+
		"mtls:\n  mode: permissive\n")
	stdout, stderr, err := executeSplit(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"), "--", "true")
	if err != nil {
		t.Fatalf("permissive mtls should run: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "mtls is configured but not enforced") {
		t.Errorf("stderr should say mtls is not enforced:\n%s", stderr)
	}
}

// TestAuthbridgeExecInboundWithoutReverseRoleWarns verifies that inbound plugins
// with no reverse listener to drive them are called out. They are built and
// started, so nothing fails — but an operator could otherwise believe their
// inbound validation is protecting something.
func TestAuthbridgeExecInboundWithoutReverseRoleWarns(t *testing.T) {
	isolateHome(t)

	cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [forward]
  forward_proxy_addr: %q
  session_api_addr: "127.0.0.1:%d"
pipeline:
  inbound:
    plugins:
      - name: inference-parser
  outbound:
    plugins:
      - name: inference-parser
`, forwardAddr(t), freePort(t)))

	stdout, stderr, err := executeSplit(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"), "--", "true")
	if err != nil {
		t.Fatalf("authbridge exec: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "inbound plugins will never run") {
		t.Errorf("stderr should warn that inbound plugins are unused:\n%s", stderr)
	}
}

// TestAuthbridgeExecReverseRoleUsesPresetAddr verifies an omitted
// reverse_proxy_addr falls back to the preset's default rather than starting
// nothing. This is the counterpart to the forward role's preset test, and is why
// the shared fixtures name an explicit port: an empty address is not "off".
func TestAuthbridgeExecReverseRoleUsesPresetAddr(t *testing.T) {
	isolateHome(t)

	// Skip rather than fail if the preset port is already taken locally: the
	// address is fixed by the preset, so this test cannot pick a free one.
	if c, err := net.DialTimeout("tcp", "127.0.0.1:8080", 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Skip("preset reverse-proxy port 8080 is already in use on this host")
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("backend-reached"))
	}))
	defer backend.Close()

	// reverse_proxy_addr is deliberately omitted — the preset supplies ":8080".
	cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [reverse]
  reverse_proxy_backend: %q
  session_api_addr: "127.0.0.1:%d"
pipeline:
  inbound:
    plugins:
      - name: inference-parser
`, backend.URL, freePort(t)))

	// The child proves the preset address is really bound, not merely printed.
	script := `curl -sf -m 5 --retry 3 --retry-connrefused --noproxy '*' http://127.0.0.1:8080/`
	stdout, stderr, err := executeSplit(t, "authbridge", "exec", "--config", cfg,
		"--logfile", filepath.Join(t.TempDir(), "authbridge.log"),
		"--", "sh", "-c", script)
	if err != nil {
		t.Fatalf("authbridge exec: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, "backend-reached") {
		t.Errorf("the preset reverse address should be bound and forwarding:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, ":8080") {
		t.Errorf("stderr should announce the preset address:\n%s", stderr)
	}
}

// TestAuthbridgeExecSessionAPIReportsInboundPipeline verifies GET /v1/pipeline
// reports the inbound composition, not just the outbound one. The session API is
// handed both holders; wired with only outbound it would report "inbound":[] for a
// config that plainly configures inbound plugins — a silent lie about what is
// running.
func TestAuthbridgeExecSessionAPIReportsInboundPipeline(t *testing.T) {
	isolateHome(t)

	apiAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	cfg := writeConfig(t, fmt.Sprintf(`mode: proxy-sidecar
listener:
  roles: [reverse]
  reverse_proxy_addr: %q
  reverse_proxy_backend: "http://127.0.0.1:1"
  session_api_addr: %q
pipeline:
  inbound:
    plugins:
      - name: inference-parser
`, forwardAddr(t), apiAddr))

	// Distinct exit codes so a failure says which half was wrong: an unreachable
	// API and an empty inbound array are different bugs.
	script := fmt.Sprintf(
		`body=$(curl -sf -m 5 --noproxy '*' http://%s/v1/pipeline) || exit 21;`+
			`printf '%%s\n' "$body";`+
			`printf '%%s' "$body" | grep -q '"inbound":\[\]' && exit 22;`+
			`printf '%%s' "$body" | grep -q 'inference-parser' || exit 23`, apiAddr)

	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg, "--", "sh", "-c", script)
	switch code {
	case 0: // inbound reported with its plugin
	case 21:
		t.Fatalf("session API /v1/pipeline did not answer at %s\n%s", apiAddr, out)
	case 22:
		t.Fatalf("/v1/pipeline reports an empty inbound pipeline; the inbound holder is not wired in\n%s", out)
	case 23:
		t.Fatalf("/v1/pipeline does not name the configured inbound plugin\n%s", out)
	default:
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
}
