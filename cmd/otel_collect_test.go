package cmd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/rossoctl-cli/internal/otelcollect"
)

// fakeRuntime installs a stub container CLI on PATH and selects it via
// $ROSSOCORTEX_RUNTIME, so `otel collect` can be run end to end with no docker or
// podman installed.
//
// The stub records its argv, one argument per line, in a file the test reads back;
// that file is the assertion target for the run command. It prints a container ID
// because Start requires one on the last line of output.
//
// Returns the path to the argv record.
func fakeRuntime(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	bin := filepath.Join(dir, "fakeruntime")

	// Truncate on `run` so a second run in the same test replaces the record
	// rather than appending to it, and append for any other verb.
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = run ]; then : > " + argvFile + "; fi\n" +
		"for a in \"$@\"; do echo \"$a\" >> " + argvFile + "; done\n" +
		"echo deadbeefcafe\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the fake runtime: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ROSSOCORTEX_RUNTIME", "fakeruntime")
	return argvFile
}

// runArgs reads the argv the fake runtime recorded.
func runArgs(t *testing.T, argvFile string) []string {
	t.Helper()
	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the fake runtime recorded nothing: %v", err)
	}
	var args []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		args = append(args, line)
	}
	return args
}

// freePorts binds the OTLP ports if something else on the machine holds them, and
// otherwise confirms they are free.
//
// `otel collect` refuses to run when 4317 or 4318 is taken, and a developer
// machine may well have a collector of its own running — so a test that ignored
// this would fail for a reason that has nothing to do with the code. Skipping is
// the honest response: the port check itself is covered by
// TestOtelCollectRefusesWhenPortsInUse, which supplies its own listener.
func requireOTLPPortsFree(t *testing.T) {
	t.Helper()
	for _, p := range []int{otelcollect.GRPCPort, otelcollect.HTTPPort} {
		if otelcollect.Listening(p) {
			t.Skipf("port %d is in use on this machine, so `otel collect` would refuse to run", p)
		}
	}
}

// pinTime fixes the timestamp in the generated filename and container name.
func pinTime(t *testing.T, at time.Time) {
	t.Helper()
	prev := timeNowOtel
	timeNowOtel = func() time.Time { return at }
	t.Cleanup(func() { timeNowOtel = prev })
}

func TestOtelIsGroup(t *testing.T) {
	out, err := execute(t, "otel")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "UNIMPLEMENTED" {
			t.Errorf("`otel` executed a stub; expected help:\n%s", out)
		}
	}
	if !strings.Contains(out, "collect") {
		t.Errorf("`otel` help missing the collect subcommand:\n%s", out)
	}
}

// TestOtelCollectRunsContainer verifies the whole command: the config is written,
// the runtime is invoked with the ports, mount, and image, and the record file is
// written.
func TestOtelCollectRunsContainer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	requireOTLPPortsFree(t)
	argvFile := fakeRuntime(t)
	pinTime(t, time.Date(2026, 8, 17, 14, 30, 45, 0, time.UTC))

	out, err := execute(t, "otel", "collect")
	if err != nil {
		t.Fatalf("otel collect: %v\n%s", err, out)
	}

	cfgPath := filepath.Join(home, ".config", "rossoctl", "otel", "collector-20260817-143045.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("collector config was not written: %v", err)
	}

	args := runArgs(t, argvFile)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run -d",
		"--name rossoctl-otelcol-20260817-143045",
		"-p " + strconv.Itoa(otelcollect.GRPCPort) + ":" + strconv.Itoa(otelcollect.GRPCPort),
		"-p " + strconv.Itoa(otelcollect.HTTPPort) + ":" + strconv.Itoa(otelcollect.HTTPPort),
		"-v " + cfgPath + ":" + otelcollect.ContainerConfigPath + ":ro",
		otelcollect.Image,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("runtime args missing %q:\n%s", want, joined)
		}
	}

	// The image must be last: anything after it is passed to the collector's own
	// entrypoint rather than to the runtime.
	if args[len(args)-1] != otelcollect.Image {
		t.Errorf("last arg = %q, want the image %q", args[len(args)-1], otelcollect.Image)
	}

	// The record names the config that was just written and the receiver endpoint.
	recordPath := filepath.Join(home, ".config", "rossoctl", otelcollect.RecordName)
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("reading %s: %v", otelcollect.RecordName, err)
	}
	var rec otelcollect.Record
	if err := yaml.Unmarshal(data, &rec); err != nil {
		t.Fatalf("%s is not valid YAML: %v\n%s", otelcollect.RecordName, err, data)
	}
	if rec.ConfigFile != cfgPath {
		t.Errorf("record configFile = %q, want %q", rec.ConfigFile, cfgPath)
	}
	// The dialable address, not the wildcard the receiver binds: this value is read
	// back and turned into a URL (an OTEL_EXPORTER_OTLP_ENDPOINT), and
	// http://0.0.0.0:4318 is not a destination.
	if rec.HTTPEndpoint != "127.0.0.1:"+strconv.Itoa(otelcollect.HTTPPort) {
		t.Errorf("record httpEndpoint = %q, want the loopback address a client dials", rec.HTTPEndpoint)
	}

	// The stop instruction has to name the container, since the run is detached
	// and not --rm.
	if !strings.Contains(out, "rossoctl-otelcol-20260817-143045") {
		t.Errorf("output does not name the container to stop:\n%s", out)
	}
}

// TestOtelCollectTracesEndpointFlag verifies the flag's value reaches the
// generated config's exporter.
func TestOtelCollectTracesEndpointFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	requireOTLPPortsFree(t)
	fakeRuntime(t)
	pinTime(t, time.Date(2026, 8, 17, 14, 30, 45, 0, time.UTC))

	const endpoint = "http://127.0.0.1:5999/v1/traces"
	if _, err := execute(t, "otel", "collect", "--traces_endpoint", endpoint); err != nil {
		t.Fatalf("otel collect: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".config", "rossoctl", "otel", "collector-20260817-143045.yaml"))
	if err != nil {
		t.Fatalf("reading the generated config: %v", err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(data, &tree); err != nil {
		t.Fatalf("generated config is not valid YAML: %v", err)
	}
	exporters := tree["exporters"].(map[string]any)
	mlflow := exporters["otlphttp/mlflow"].(map[string]any)
	if mlflow["traces_endpoint"] != endpoint {
		t.Errorf("traces_endpoint = %v, want %q", mlflow["traces_endpoint"], endpoint)
	}
}

// TestOtelCollectDefaultTracesEndpoint verifies the documented default is what is
// used when the flag is absent.
func TestOtelCollectDefaultTracesEndpoint(t *testing.T) {
	f := otelCollectCmd.Flags().Lookup("traces_endpoint")
	if f == nil {
		t.Fatal("otel collect has no --traces_endpoint flag")
	}
	if f.DefValue != "http://host.containers.internal:5001/v1/traces" {
		t.Errorf("--traces_endpoint default = %q, want the host.containers.internal URL", f.DefValue)
	}
}

// TestOtelCollectWarnsWhenMLflowAbsent verifies the warning names the port and
// suggests the mlflow command, and that the collector still starts.
func TestOtelCollectWarnsWhenMLflowAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	requireOTLPPortsFree(t)
	fakeRuntime(t)

	// A port that was just released, so nothing is listening on it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	endpoint := "http://host.containers.internal:" + strconv.Itoa(port) + "/v1/traces"
	stdout, stderr, err := executeSplit(t, "otel", "collect", "--traces_endpoint", endpoint)
	if err != nil {
		t.Fatalf("otel collect: %v", err)
	}

	for _, want := range []string{
		"nothing is listening",
		strconv.Itoa(port),
		"mlflow server",
		"--host 0.0.0.0",
		"--allowed-hosts",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	// The warning is advice, not a failure: MLflow can be started afterwards and
	// the exporter retries, so the collector must still come up.
	if !strings.Contains(stdout, "started") {
		t.Errorf("the collector should still start when MLflow is absent:\n%s", stdout)
	}
}

// TestOtelCollectQuietWhenMLflowPresent verifies the warning is skipped when
// something is listening.
func TestOtelCollectQuietWhenMLflowPresent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	requireOTLPPortsFree(t)
	fakeRuntime(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	endpoint := "http://host.containers.internal:" + strconv.Itoa(port) + "/v1/traces"
	_, stderr, err := executeSplit(t, "otel", "collect", "--traces_endpoint", endpoint)
	if err != nil {
		t.Fatalf("otel collect: %v", err)
	}
	if strings.Contains(stderr, "nothing is listening") {
		t.Errorf("warned about a port that has a listener:\n%s", stderr)
	}
}

// TestOtelCollectRefusesWhenPortsInUse verifies an OTLP port already in use fails
// the command before anything is written.
//
// The no-file assertion is the substance: the ports are published on fixed host
// numbers, so the runtime would refuse at `run` — after a config file had been
// written for a collector that never started.
func TestOtelCollectRefusesWhenPortsInUse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	fakeRuntime(t)

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(otelcollect.HTTPPort))
	if err != nil {
		t.Skipf("cannot bind %d to simulate a conflict: %v", otelcollect.HTTPPort, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	_, err = execute(t, "otel", "collect")
	if err == nil {
		t.Fatal("expected an error when an OTLP port is already in use")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(otelcollect.HTTPPort)) {
		t.Errorf("error should name the port in use: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".config", "rossoctl", "otel")); !os.IsNotExist(statErr) {
		t.Errorf("no config should have been written when the ports are taken (stat: %v)", statErr)
	}
}

// TestOtelCollectRejectsBadEndpoint verifies a malformed --traces_endpoint fails
// before anything is written or started.
func TestOtelCollectRejectsBadEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	argvFile := fakeRuntime(t)

	// No scheme, so the value has no host and no port to probe.
	_, err := execute(t, "otel", "collect", "--traces_endpoint", "host.containers.internal:5001")
	if err == nil {
		t.Fatal("expected an error for an endpoint with no scheme")
	}
	if !strings.Contains(err.Error(), "traces_endpoint") {
		t.Errorf("error should name the flag: %v", err)
	}
	if _, statErr := os.Stat(argvFile); !os.IsNotExist(statErr) {
		t.Error("the runtime should not have been invoked for a bad endpoint")
	}
	if _, statErr := os.Stat(filepath.Join(home, ".config", "rossoctl", "otel")); !os.IsNotExist(statErr) {
		t.Error("no config should have been written for a bad endpoint")
	}
}

// TestOtelCollectConfigIsUnderHome verifies the generated config is written inside
// the home directory even when XDG_CONFIG_HOME points elsewhere, and that the
// override is reported.
//
// This is the mountability constraint: a container runtime on macOS or Windows can
// only bind-mount host paths its VM shares, which by default is the home
// directory. A config outside it would be written and then fail to mount.
func TestOtelCollectConfigIsUnderHome(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", outside)
	requireOTLPPortsFree(t)
	argvFile := fakeRuntime(t)

	_, stderr, err := executeSplit(t, "otel", "collect")
	if err != nil {
		t.Fatalf("otel collect: %v", err)
	}
	if !strings.Contains(stderr, "XDG_CONFIG_HOME") {
		t.Errorf("the ignored XDG_CONFIG_HOME should be reported:\n%s", stderr)
	}

	// The mounted path is the one that matters, so assert on what was passed to -v.
	args := runArgs(t, argvFile)
	var mount string
	for i, a := range args {
		if a == "-v" && i+1 < len(args) {
			mount = args[i+1]
		}
	}
	if mount == "" {
		t.Fatalf("no -v argument in the runtime invocation: %v", args)
	}
	if !strings.HasPrefix(mount, home) {
		t.Errorf("mounted %q, which is not under the home directory %q", mount, home)
	}
	if strings.HasPrefix(mount, outside) {
		t.Errorf("mounted %q from outside the home directory, which a runtime may not share", mount)
	}
}

// TestOtelCollectMountsReadOnly verifies the config is mounted read-only: the
// collector reads it and has no reason to write back to the host.
func TestOtelCollectMountsReadOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	requireOTLPPortsFree(t)
	argvFile := fakeRuntime(t)

	if _, err := execute(t, "otel", "collect"); err != nil {
		t.Fatalf("otel collect: %v", err)
	}

	args := runArgs(t, argvFile)
	for i, a := range args {
		if a == "-v" && i+1 < len(args) {
			if !strings.HasSuffix(args[i+1], ":ro") {
				t.Errorf("mount %q is not read-only", args[i+1])
			}
			return
		}
	}
	t.Fatalf("no -v argument in the runtime invocation: %v", args)
}

// TestOtelCollectAddsHostGateway verifies the container can resolve the hostname
// the default endpoint names.
//
// host.containers.internal is podman's own name for the host; docker does not
// always provide it, so it is added explicitly and the default endpoint works
// under both runtimes.
func TestOtelCollectAddsHostGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	requireOTLPPortsFree(t)
	argvFile := fakeRuntime(t)

	if _, err := execute(t, "otel", "collect"); err != nil {
		t.Fatalf("otel collect: %v", err)
	}

	joined := strings.Join(runArgs(t, argvFile), " ")
	if !strings.Contains(joined, "--add-host host.containers.internal:host-gateway") {
		t.Errorf("runtime args should map host.containers.internal to the host gateway:\n%s", joined)
	}
}

// TestOtelCollectRejectsArgs verifies the command takes no positional arguments,
// so a mistyped flag is reported rather than ignored.
func TestOtelCollectRejectsArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := execute(t, "otel", "collect", "unexpected"); err == nil {
		t.Error("expected an error for a positional argument")
	}
}
