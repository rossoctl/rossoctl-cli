package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/otelcollect"
)

// writeOtelRecord writes an otel-config.yaml under the isolated home, as
// `otel collect` would, and returns its path.
func writeOtelRecord(t *testing.T, httpEndpoint string) string {
	t.Helper()
	path, err := otelCollectRecordPath()
	if err != nil {
		t.Fatalf("otelCollectRecordPath: %v", err)
	}
	if err := otelcollect.WriteRecord(path, otelcollect.Record{
		ConfigFile:   filepath.Join(filepath.Dir(path), "otel", "collector-test.yaml"),
		HTTPEndpoint: httpEndpoint,
	}); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	return path
}

// childEnvFor runs `authbridge exec --with-claude-otel` with a command that prints
// its environment, and returns that environment as a name->value map.
//
// The child is `env`, so what is asserted is the environment the command actually
// received rather than an intermediate slice.
func childEnvFor(t *testing.T, extraArgs ...string) map[string]string {
	t.Helper()
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	args := append([]string{"authbridge", "exec", "--config", cfg}, extraArgs...)
	args = append(args, "--", "env")

	out, code := execExitCode(t, args...)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	env := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			env[k] = v
		}
	}
	return env
}

// TestExecWithClaudeOtelSetsEnv verifies the command receives every documented
// variable, with the endpoint built from the record's httpEndpoint.
func TestExecWithClaudeOtelSetsEnv(t *testing.T) {
	isolateHome(t)
	writeOtelRecord(t, "127.0.0.1:4318")

	env := childEnvFor(t, "--with-claude-otel")

	for k, want := range map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":        "1",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
		"OTEL_EXPORTER_OTLP_PROTOCOL":         "http/json",
		"OTEL_TRACES_EXPORT_INTERVAL":         "1",
		"OTEL_TRACES_EXPORTER":                "otlp",
		"OTEL_LOGS_EXPORTER":                  "none",
		"OTEL_METRICS_EXPORTER":               "none",
		"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://127.0.0.1:4318",
	} {
		if got, ok := env[k]; !ok {
			t.Errorf("%s was not set on the command", k)
		} else if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// TestExecWithClaudeOtelKeepsInheritedEnvironment verifies the child still gets the
// ordinary environment.
//
// The substance: the host used here runs no forward proxy, so host.env is nil, which
// exec.Cmd reads as "inherit". Appending to that nil without expanding it first would
// hand the child *only* the telemetry variables — no PATH, no HOME.
func TestExecWithClaudeOtelKeepsInheritedEnvironment(t *testing.T) {
	isolateHome(t)
	writeOtelRecord(t, "127.0.0.1:4318")
	t.Setenv("ROSSOCTL_OTEL_CANARY", "still-here")

	env := childEnvFor(t, "--with-claude-otel")

	if env["ROSSOCTL_OTEL_CANARY"] != "still-here" {
		t.Errorf("an inherited variable was lost: ROSSOCTL_OTEL_CANARY = %q", env["ROSSOCTL_OTEL_CANARY"])
	}
	if env["PATH"] == "" {
		t.Error("PATH was lost; the inherited environment must be preserved")
	}
}

// TestExecWithClaudeOtelUsesRecordedEndpoint verifies the endpoint tracks the record
// rather than being hardcoded.
func TestExecWithClaudeOtelUsesRecordedEndpoint(t *testing.T) {
	isolateHome(t)
	writeOtelRecord(t, "127.0.0.1:14318")

	env := childEnvFor(t, "--with-claude-otel")

	if got, want := env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:14318"; got != want {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want %q from the record", got, want)
	}
}

// TestExecWithClaudeOtelRewritesWildcardEndpoint verifies a record carrying the
// receiver's wildcard bind address still yields a dialable URL.
//
// Covers a record written before that was corrected, or edited by hand: an exporter
// told to send to http://0.0.0.0:4318 relies on the platform reinterpreting it.
func TestExecWithClaudeOtelRewritesWildcardEndpoint(t *testing.T) {
	isolateHome(t)
	writeOtelRecord(t, "0.0.0.0:4318")

	env := childEnvFor(t, "--with-claude-otel")

	if got, want := env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:4318"; got != want {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want the rewritten %q", got, want)
	}
}

// TestExecWithoutClaudeOtelSetsNothing verifies the variables appear only when the
// flag is given.
func TestExecWithoutClaudeOtelSetsNothing(t *testing.T) {
	isolateHome(t)
	writeOtelRecord(t, "127.0.0.1:4318")

	env := childEnvFor(t)

	for _, k := range []string{
		"CLAUDE_CODE_ENABLE_TELEMETRY",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_TRACES_EXPORTER",
	} {
		if v, ok := env[k]; ok {
			t.Errorf("%s = %q was set without --with-claude-otel", k, v)
		}
	}
}

// TestExecWithClaudeOtelKeepsInheritedOtelVars verifies an already-exported telemetry
// variable is left alone and reported, as the proxy and CA variables are.
//
// Someone who exported OTEL_EXPORTER_OTLP_ENDPOINT before running this has said where
// they want spans to go.
func TestExecWithClaudeOtelKeepsInheritedOtelVars(t *testing.T) {
	isolateHome(t)
	writeOtelRecord(t, "127.0.0.1:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.example.com:4318")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")

	cfg := writeConfig(t, pipelineOnlyConfig(t))
	out, code := execExitCode(t, "authbridge", "exec", "--config", cfg,
		"--with-claude-otel", "--", "env")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}

	env := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			env[k] = v
		}
	}

	if got := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != "http://collector.example.com:4318" {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want the inherited value kept", got)
	}
	if got := env["OTEL_METRICS_EXPORTER"]; got != "otlp" {
		t.Errorf("OTEL_METRICS_EXPORTER = %q, want the inherited value kept", got)
	}
	// The override is announced, so a user is not left wondering why spans went
	// somewhere else.
	if !strings.Contains(out, "keeping inherited OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Errorf("the kept variable should be reported:\n%s", out)
	}
	// A variable the environment did not already set is still applied.
	if got := env["OTEL_TRACES_EXPORTER"]; got != "otlp" {
		t.Errorf("OTEL_TRACES_EXPORTER = %q, want otlp", got)
	}
}

// TestExecWithClaudeOtelRequiresRecord verifies the flag fails, naming the command
// that creates the record, when no collector has been started.
//
// Failing rather than exporting a guessed endpoint is the point: a child pointed at a
// collector that is not there buffers and drops spans silently.
func TestExecWithClaudeOtelRequiresRecord(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	out, err := execute(t, "authbridge", "exec", "--config", cfg,
		"--with-claude-otel", "--", "true")
	if err == nil {
		t.Fatalf("expected an error when no otel record exists\n%s", out)
	}
	if !strings.Contains(err.Error(), "otel collect") {
		t.Errorf("error should name the command that starts a collector: %v", err)
	}
	if !strings.Contains(err.Error(), otelcollect.RecordName) {
		t.Errorf("error should name the missing record file: %v", err)
	}
}

// TestExecWithClaudeOtelRejectsEmptyRecordEndpoint verifies a record with no
// httpEndpoint fails rather than exporting "http://".
func TestExecWithClaudeOtelRejectsEmptyRecordEndpoint(t *testing.T) {
	isolateHome(t)
	recordPath, err := otelCollectRecordPath()
	if err != nil {
		t.Fatalf("otelCollectRecordPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(recordPath, []byte("configFile: /tmp/c.yaml\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cfg := writeConfig(t, pipelineOnlyConfig(t))
	out, execErr := execute(t, "authbridge", "exec", "--config", cfg,
		"--with-claude-otel", "--", "true")
	if execErr == nil {
		t.Fatalf("expected an error for a record with no httpEndpoint\n%s", out)
	}
	if !strings.Contains(execErr.Error(), "httpEndpoint") {
		t.Errorf("error should name the missing field: %v", execErr)
	}
}

// TestExecWithClaudeOtelFlagSurface verifies the flag is registered as a bool and
// documented.
func TestExecWithClaudeOtelFlagSurface(t *testing.T) {
	f := authbridgeExecCmd.Flags().Lookup("with-claude-otel")
	if f == nil {
		t.Fatal("authbridge exec has no --with-claude-otel flag")
	}
	if f.Value.Type() != "bool" {
		t.Errorf("--with-claude-otel is a %s, want bool", f.Value.Type())
	}
	if f.DefValue != "false" {
		t.Errorf("--with-claude-otel default = %q, want false", f.DefValue)
	}

	out, err := execute(t, "authbridge", "exec", "--help")
	if err != nil {
		t.Fatalf("exec --help: %v", err)
	}
	// The help has to name the variables, since they are the reason to use the flag.
	for _, want := range []string{
		"--with-claude-otel",
		"CLAUDE_CODE_ENABLE_TELEMETRY",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		otelcollect.RecordName,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exec --help does not mention %q", want)
		}
	}
}

// TestExecOtelEndpointOverridesRecord verifies --otel-endpoint replaces the endpoint
// the record names, leaving the other telemetry variables alone.
func TestExecOtelEndpointOverridesRecord(t *testing.T) {
	isolateHome(t)
	writeOtelRecord(t, "127.0.0.1:4318")

	env := childEnvFor(t, "--with-claude-otel", "--otel-endpoint", "http://collector.example.com:4318")

	if got, want := env["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://collector.example.com:4318"; got != want {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want %q from --otel-endpoint", got, want)
	}
	// The rest of the set is unaffected: the flag names an endpoint, not a policy.
	if got := env["OTEL_TRACES_EXPORTER"]; got != "otlp" {
		t.Errorf("OTEL_TRACES_EXPORTER = %q, want otlp", got)
	}
	if got := env["CLAUDE_CODE_ENABLE_TELEMETRY"]; got != "1" {
		t.Errorf("CLAUDE_CODE_ENABLE_TELEMETRY = %q, want 1", got)
	}
}

// TestExecOtelEndpointWithoutRecord verifies the override needs no local collector.
//
// This is the case it exists for: pointing at a collector this host did not start,
// where insisting on a record would defeat the flag.
func TestExecOtelEndpointWithoutRecord(t *testing.T) {
	isolateHome(t)
	// Deliberately no writeOtelRecord.

	env := childEnvFor(t, "--with-claude-otel", "--otel-endpoint", "https://otel.example.com")

	if got, want := env["OTEL_EXPORTER_OTLP_ENDPOINT"], "https://otel.example.com"; got != want {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want %q", got, want)
	}
}

// TestExecOtelEndpointIsSentVerbatim verifies the value is exported exactly as typed.
//
// No scheme rewriting, no trailing-slash normalization, and above all no path
// appended: OTEL_EXPORTER_OTLP_ENDPOINT is a base URL the SDK adds the signal path
// to, so anything added here would be sent to /v1/traces/v1/traces.
func TestExecOtelEndpointIsSentVerbatim(t *testing.T) {
	isolateHome(t)

	for _, endpoint := range []string{
		"http://127.0.0.1:14318",
		"https://otel.example.com:443",
		"http://otel.internal",
		// A path is a legitimate thing to name — a collector behind a prefix — and
		// must survive as given.
		"http://gateway.example.com/otlp",
	} {
		t.Run(endpoint, func(t *testing.T) {
			env := childEnvFor(t, "--with-claude-otel", "--otel-endpoint", endpoint)
			if got := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != endpoint {
				t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want %q verbatim", got, endpoint)
			}
		})
	}
}

// TestExecOtelEndpointRequiresWithClaudeOtel verifies the flag is rejected on its own.
func TestExecOtelEndpointRequiresWithClaudeOtel(t *testing.T) {
	isolateHome(t)
	writeOtelRecord(t, "127.0.0.1:4318")
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	out, err := execute(t, "authbridge", "exec", "--config", cfg,
		"--otel-endpoint", "http://127.0.0.1:4318", "--", "true")
	if err == nil {
		t.Fatalf("expected an error for --otel-endpoint without --with-claude-otel\n%s", out)
	}
	if !strings.Contains(err.Error(), "--with-claude-otel") {
		t.Errorf("error should name the flag it requires: %v", err)
	}
}

// TestExecOtelEndpointRejectsNonURL verifies a value that is not a full URL fails.
//
// A bare host:port is the one worth pinning: url.Parse accepts it as a URL whose
// scheme is the host, so without the scheme check it would be exported unusable.
func TestExecOtelEndpointRejectsNonURL(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	for _, tc := range []struct {
		name     string
		endpoint string
	}{
		{"bare host and port", "127.0.0.1:4318"},
		{"host only", "collector.example.com"},
		{"no scheme, leading slashes", "//collector.example.com:4318"},
		{"unsupported scheme", "grpc://127.0.0.1:4317"},
		{"scheme but no host", "http://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execute(t, "authbridge", "exec", "--config", cfg,
				"--with-claude-otel", "--otel-endpoint", tc.endpoint, "--", "true")
			if err == nil {
				t.Fatalf("expected an error for --otel-endpoint %q\n%s", tc.endpoint, out)
			}
			if !strings.Contains(err.Error(), "otel-endpoint") {
				t.Errorf("error should name the flag: %v", err)
			}
		})
	}
}

// TestExecOtelEndpointEmptyValueIsRejected verifies an explicitly empty value is an
// error rather than silently falling back to the record.
//
// `--otel-endpoint ""` is a stated intention that cannot be honored; treating it as
// "unset" would quietly point the child somewhere the user did not name.
func TestExecOtelEndpointEmptyValueIsRejected(t *testing.T) {
	isolateHome(t)
	writeOtelRecord(t, "127.0.0.1:4318")
	cfg := writeConfig(t, pipelineOnlyConfig(t))

	out, err := execute(t, "authbridge", "exec", "--config", cfg,
		"--with-claude-otel", "--otel-endpoint", "", "--", "true")
	if err == nil {
		t.Fatalf("expected an error for an empty --otel-endpoint\n%s", out)
	}
	if !strings.Contains(err.Error(), "otel-endpoint") {
		t.Errorf("error should name the flag: %v", err)
	}
}

// TestExecOtelEndpointFlagSurface verifies the flag's registration and that the help
// documents it.
func TestExecOtelEndpointFlagSurface(t *testing.T) {
	f := authbridgeExecCmd.Flags().Lookup("otel-endpoint")
	if f == nil {
		t.Fatal("authbridge exec has no --otel-endpoint flag")
	}
	if f.Value.Type() != "string" {
		t.Errorf("--otel-endpoint is a %s, want string", f.Value.Type())
	}
	if f.DefValue != "" {
		t.Errorf("--otel-endpoint default = %q, want empty", f.DefValue)
	}

	out, err := execute(t, "authbridge", "exec", "--help")
	if err != nil {
		t.Fatalf("exec --help: %v", err)
	}
	if !strings.Contains(out, "--otel-endpoint") {
		t.Errorf("exec --help does not document --otel-endpoint:\n%s", out)
	}
}
