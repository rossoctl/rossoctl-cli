package containers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// fakeRun records the arguments each invocation received and replies from a
// scripted table keyed by the subcommand, so a test can drive cliEngine without
// a container runtime installed.
type fakeRun struct {
	// replies maps a subcommand ("run", "stop", "inspect") to its output.
	replies map[string]string
	// errs maps a subcommand to an error to return instead.
	errs map[string]error
	// calls records every argument list passed, in order.
	calls [][]string
}

func (f *fakeRun) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if len(args) == 0 {
		return nil, errors.New("no args")
	}
	sub := args[0]
	if err, ok := f.errs[sub]; ok {
		return []byte(f.replies[sub]), err
	}
	return []byte(f.replies[sub]), nil
}

// engineWith returns a cliEngine wired to f.
func engineWith(f *fakeRun) *cliEngine {
	return &cliEngine{bin: "docker", run: f.run}
}

// argsFor returns the recorded argument list for the given subcommand.
func (f *fakeRun) argsFor(t *testing.T, sub string) []string {
	t.Helper()
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == sub {
			return c
		}
	}
	t.Fatalf("no %q call recorded; calls=%v", sub, f.calls)
	return nil
}

// joined renders an argument list as one space-separated string, for asserting
// that a flag and its value are adjacent rather than merely both present.
func joined(args []string) string { return strings.Join(args, " ") }

// TestStartBuildsRunCommand verifies the run command carries the detached flag
// (but not --rm), one -p per published port, the mounts, and the container's own
// arguments after the image.
func TestStartBuildsRunCommand(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"run": "abc123def4567890\n"}}
	e := engineWith(f)

	id, err := e.Start(context.Background(), RunSpec{
		Image:        "example/proxy:v1",
		PublishPorts: []int{8080, 9094},
		Mounts: []Mount{
			{HostPath: "/host/config.yaml", ContainerPath: "/tmp/config.yaml", ReadOnly: true},
			{HostPath: "/host/ca", ContainerPath: "/etc/ca"},
		},
		HostEntries: []HostEntry{{Name: "keycloak.localtest.me", Address: HostGateway}},
		Env:         []string{"LOG_LEVEL=debug"},
		Args:        []string{"--config", "/tmp/config.yaml"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id != "abc123def4567890" {
		t.Errorf("id = %q, want the container ID", id)
	}

	got := joined(f.argsFor(t, "run"))
	for _, want := range []string{
		"run -d",
		"-p 8080",
		"-p 9094",
		"-v /host/config.yaml:/tmp/config.yaml:ro",
		"-v /host/ca:/etc/ca",
		"--add-host keycloak.localtest.me:host-gateway",
		"-e LOG_LEVEL=debug",
		// The image must precede the container's own args, or they would be
		// parsed as flags of the runtime's.
		"example/proxy:v1 --config /tmp/config.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("run args %q missing %q", got, want)
		}
	}

	// Explicitly not --rm: an exited container must survive so its logs can be
	// read, and Stop removes it instead. Checked as a negative because "run -d"
	// above is a substring of "run -d --rm" and would not catch a regression.
	if strings.Contains(got, "--rm") {
		t.Errorf("run args %q must not use --rm: it reaps a crashed container and its logs", got)
	}
}

// TestStartHostEntries verifies each host entry becomes its own --add-host, in
// the order given, and that an entry's name and address are joined with a colon.
func TestStartHostEntries(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"run": "abc123\n"}}
	_, err := engineWith(f).Start(context.Background(), RunSpec{
		Image: "example/proxy:v1",
		HostEntries: []HostEntry{
			{Name: "keycloak.localtest.me", Address: HostGateway},
			{Name: "api.internal", Address: "10.0.0.7"},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// One flag per entry rather than one flag with a joined value: --add-host
	// takes a single mapping, so a combined value would be parsed as one absurd
	// hostname.
	got := joined(f.argsFor(t, "run"))
	want := "--add-host keycloak.localtest.me:host-gateway --add-host api.internal:10.0.0.7"
	if !strings.Contains(got, want) {
		t.Errorf("run args %q missing %q", got, want)
	}

	// Before the image, or the runtime would treat them as the container's own
	// arguments instead of its flags.
	if strings.Index(got, "--add-host") > strings.Index(got, "example/proxy:v1") {
		t.Errorf("run args %q put --add-host after the image", got)
	}
}

// TestStartNoHostEntries verifies no --add-host appears when none were asked
// for, so the common case does not make a claim about name resolution.
func TestStartNoHostEntries(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"run": "abc123\n"}}
	if _, err := engineWith(f).Start(context.Background(), RunSpec{Image: "example/proxy:v1"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := joined(f.argsFor(t, "run")); strings.Contains(got, "--add-host") {
		t.Errorf("run args %q added a host entry when none was requested", got)
	}
}

// TestStartIDAfterPullProgress verifies the container ID is taken from the last
// line: with combined output, an image pull's progress precedes it.
func TestStartIDAfterPullProgress(t *testing.T) {
	f := &fakeRun{replies: map[string]string{
		"run": "Unable to find image locally\nlatest: Pulling from example/proxy\nDigest: sha256:x\nfeedfacefeedface\n",
	}}
	id, err := engineWith(f).Start(context.Background(), RunSpec{Image: "example/proxy:v1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id != "feedfacefeedface" {
		t.Errorf("id = %q, want the last line", id)
	}
}

// TestStartRequiresImage verifies an empty image is rejected before the runtime
// is invoked at all.
func TestStartRequiresImage(t *testing.T) {
	f := &fakeRun{}
	if _, err := engineWith(f).Start(context.Background(), RunSpec{}); err == nil {
		t.Fatal("expected an error for an empty image")
	}
	if len(f.calls) != 0 {
		t.Errorf("runtime invoked %d times for an empty image, want 0", len(f.calls))
	}
}

// TestStartNoIDIsError verifies that a run reporting success but printing no ID
// is an error rather than a container tracked by an empty string.
func TestStartNoIDIsError(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"run": "\n  \n"}}
	if _, err := engineWith(f).Start(context.Background(), RunSpec{Image: "x"}); err == nil {
		t.Fatal("expected an error when run prints no container ID")
	}
}

// TestInspectParsesPorts verifies the port map is parsed from the JSON the
// --format template produces, including a published-but-unbound port.
func TestInspectParsesPorts(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"inspect": `{"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"49154"},{"HostIp":"::","HostPort":"49154"}],"9094/tcp":[{"HostIp":"127.0.0.1","HostPort":"49155"}],"9093/tcp":null}` + "\n"}}
	e := engineWith(f)

	ports, err := e.Inspect(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	// The format template is what makes the output parseable, so assert it is
	// actually the one requested.
	if got := joined(f.argsFor(t, "inspect")); !strings.Contains(got, "--format {{json .NetworkSettings.Ports}}") {
		t.Errorf("inspect args %q missing the JSON ports format", got)
	}

	if p, ok := HostPort(ports, 8080); !ok || p != 49154 {
		t.Errorf("HostPort(8080) = (%d, %v), want (49154, true)", p, ok)
	}
	if p, ok := HostPort(ports, 9094); !ok || p != 49155 {
		t.Errorf("HostPort(9094) = (%d, %v), want (49155, true)", p, ok)
	}
	// Published but unbound, and never published at all, both mean "no host
	// port" — the caller cannot dial either.
	if _, ok := HostPort(ports, 9093); ok {
		t.Error("HostPort(9093) reported a port for a null binding")
	}
	if _, ok := HostPort(ports, 1234); ok {
		t.Error("HostPort(1234) reported a port for an unpublished container port")
	}
}

// TestHostPortPrefersIPv4 verifies an IPv4 binding wins over an IPv6 one, since
// that is what a client reliably reaches — even when the IPv6 entry comes first.
func TestHostPortPrefersIPv4(t *testing.T) {
	ports := map[string][]PortBinding{
		"8080/tcp": {
			{HostIP: "::", HostPort: "1111"},
			{HostIP: "0.0.0.0", HostPort: "2222"},
		},
	}
	if p, ok := HostPort(ports, 8080); !ok || p != 2222 {
		t.Errorf("HostPort = (%d, %v), want (2222, true)", p, ok)
	}

	// IPv6-only still yields a port rather than nothing: it is better to try it
	// than to claim the port was never published.
	only := map[string][]PortBinding{"8080/tcp": {{HostIP: "::", HostPort: "3333"}}}
	if p, ok := HostPort(only, 8080); !ok || p != 3333 {
		t.Errorf("HostPort(IPv6 only) = (%d, %v), want (3333, true)", p, ok)
	}
}

// TestInspectEmptyPortMap verifies a container with no published ports is not an
// error: the caller decides whether it needed one.
func TestInspectEmptyPortMap(t *testing.T) {
	for _, out := range []string{"null\n", "{}\n", "\n"} {
		f := &fakeRun{replies: map[string]string{"inspect": out}}
		ports, err := engineWith(f).Inspect(context.Background(), "abc")
		if err != nil {
			t.Fatalf("Inspect(%q): %v", out, err)
		}
		if len(ports) != 0 {
			t.Errorf("Inspect(%q) = %v, want an empty map", out, ports)
		}
	}
}

// TestInspectBadJSONIsError verifies unparseable output is reported rather than
// silently yielding an empty port map, which would look like "no ports".
func TestInspectBadJSONIsError(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"inspect": "not json\n"}}
	if _, err := engineWith(f).Inspect(context.Background(), "abc"); err == nil {
		t.Fatal("expected an error for unparseable inspect output")
	}
}

// TestStopPassesTimeout verifies Stop asks the runtime for a graceful stop with
// the documented budget.
func TestStopPassesTimeout(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"stop": "abc123\n"}}
	if err := engineWith(f).Stop(context.Background(), "abc123"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	want := fmt.Sprintf("stop -t %d abc123", int(StopTimeout.Seconds()))
	if got := joined(f.argsFor(t, "stop")); got != want {
		t.Errorf("stop args = %q, want %q", got, want)
	}
}

// TestStopAbsentContainerIsNotAnError verifies that stopping a container which is
// already gone succeeds: the postcondition is that it is not running, and it is
// not. A container someone removed by hand between Start and Stop hits this.
func TestStopAbsentContainerIsNotAnError(t *testing.T) {
	for _, msg := range []string{
		"Error response from daemon: No such container: abc123",
		"Error: no container with name or ID \"abc123\" found",
	} {
		f := &fakeRun{
			replies: map[string]string{"stop": msg},
			errs:    map[string]error{"stop": errors.New("exit status 1")},
		}
		if err := engineWith(f).Stop(context.Background(), "abc123"); err != nil {
			t.Errorf("Stop(%q) = %v, want nil", msg, err)
		}
	}
}

// TestStopRealFailureIsReported verifies an actual stop failure is not swallowed
// by the already-gone tolerance.
func TestStopRealFailureIsReported(t *testing.T) {
	f := &fakeRun{
		replies: map[string]string{"stop": "permission denied while trying to connect to the Docker daemon"},
		errs:    map[string]error{"stop": errors.New("exit status 1")},
	}
	if err := engineWith(f).Stop(context.Background(), "abc123"); err == nil {
		t.Fatal("expected a real stop failure to be reported")
	}
}

// TestStopSurvivesCancelledContext verifies Stop still runs when the context it
// is handed is already cancelled. Cleanup is exactly when that happens — the
// surrounding operation is ending — and inheriting the cancellation would turn
// the stop into a no-op and leak the container.
func TestStopSurvivesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := &fakeRun{replies: map[string]string{"stop": "abc123\n"}}
	if err := engineWith(f).Stop(ctx, "abc123"); err != nil {
		t.Fatalf("Stop with a cancelled context: %v", err)
	}
	f.argsFor(t, "stop") // fails the test if the stop never happened
}

// TestStopAndInspectRequireID verifies an empty ID is rejected before the runtime
// is invoked: `docker stop ""` would be a confusing way to learn this.
func TestStopAndInspectRequireID(t *testing.T) {
	f := &fakeRun{}
	e := engineWith(f)
	if err := e.Stop(context.Background(), ""); err == nil {
		t.Error("expected an error from Stop with an empty ID")
	}
	if _, err := e.Inspect(context.Background(), ""); err == nil {
		t.Error("expected an error from Inspect with an empty ID")
	}
	if len(f.calls) != 0 {
		t.Errorf("runtime invoked %d times for an empty ID, want 0", len(f.calls))
	}
}

// TestMountArg covers the -v rendering, including the read-only suffix.
func TestMountArg(t *testing.T) {
	tests := []struct {
		mount Mount
		want  string
	}{
		{Mount{HostPath: "/a", ContainerPath: "/b"}, "/a:/b"},
		{Mount{HostPath: "/a", ContainerPath: "/b", ReadOnly: true}, "/a:/b:ro"},
	}
	for _, tc := range tests {
		if got := tc.mount.arg(); got != tc.want {
			t.Errorf("Mount%+v.arg() = %q, want %q", tc.mount, got, tc.want)
		}
	}
}

// TestDetectHonorsRuntimeOverride verifies $ROSSOCORTEX_RUNTIME selects the
// runtime, and that an unset-and-absent runtime is a clear error rather than a
// nil engine handed back to the caller.
func TestDetectHonorsRuntimeOverride(t *testing.T) {
	// An empty PATH means nothing resolves, so Detect must fail rather than
	// return an engine that cannot run anything.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ROSSOCORTEX_RUNTIME", "")
	if _, _, err := Detect(); err == nil {
		t.Fatal("expected an error when no runtime is on PATH")
	}

	// A runtime that does exist on PATH is chosen, and named as asked for.
	dir := t.TempDir()
	fake := dir + "/mycontainerd"
	if err := writeExecutable(fake); err != nil {
		t.Fatalf("writing fake runtime: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ROSSOCORTEX_RUNTIME", "mycontainerd")

	engine, bin, err := Detect()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if bin != "mycontainerd" {
		t.Errorf("bin = %q, want mycontainerd", bin)
	}
	if engine == nil {
		t.Error("Detect returned a nil engine with no error")
	}
}

// writeExecutable creates an executable file at path, so exec.LookPath resolves
// it. Its contents do not matter: Detect only checks that it is executable.
func writeExecutable(path string) error {
	return os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755)
}

// TestLastLine covers the output-scraping helper, whose job is to skip a
// runtime's progress and warning lines.
func TestLastLine(t *testing.T) {
	tests := []struct{ in, want string }{
		{"abc\n", "abc"},
		{"pulling...\nabc\n\n", "abc"},
		{"  abc  ", "abc"},
		{"", ""},
		{"\n \n", ""},
	}
	for _, tc := range tests {
		if got := lastLine(tc.in); got != tc.want {
			t.Errorf("lastLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestExecLogsCommandLine covers the --verbose command-line logging: every
// runtime invocation is logged, in order, and the line names the binary and its
// arguments so it can be rerun by hand.
func TestExecLogsCommandLine(t *testing.T) {
	f := &fakeRun{replies: map[string]string{
		"run":     "container-id\n",
		"inspect": `{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"32770"}]}`,
	}}
	e := engineWith(f)

	var logs []string
	SetLogf(e, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	if _, err := e.Start(context.Background(), RunSpec{Image: "img:latest"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.Inspect(context.Background(), "container-id"); err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if len(logs) != 2 {
		t.Fatalf("expected one log line per invocation, got %d: %v", len(logs), logs)
	}
	if !strings.HasPrefix(logs[0], "docker run ") {
		t.Errorf("run log = %q, want it to start with the binary and subcommand", logs[0])
	}
	if !strings.Contains(logs[0], "img:latest") {
		t.Errorf("run log = %q, want it to name the image", logs[0])
	}
	if !strings.HasPrefix(logs[1], "docker inspect ") {
		t.Errorf("inspect log = %q, want it to start with the binary and subcommand", logs[1])
	}
}

// TestExecLogsBeforeRunning verifies the command line is logged before the
// runtime is invoked, not after. A command that hangs or fails is exactly when
// seeing it matters, so logging must not depend on the call returning.
func TestExecLogsBeforeRunning(t *testing.T) {
	var logged bool
	e := &cliEngine{
		bin: "docker",
		run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			if !logged {
				t.Error("the command line was not logged before the runtime ran")
			}
			return nil, errors.New("boom")
		},
		logf: func(string, ...any) { logged = true },
	}

	// The error is the fake's, and is expected; the assertion is inside run.
	if _, err := e.exec(context.Background(), "run", "img"); err == nil {
		t.Fatal("expected the fake runtime's error")
	}
	if !logged {
		t.Error("nothing was logged")
	}
}

// TestExecNilLogfIsSafe verifies the quiet path: with no logf set (the default,
// and what a non-verbose run leaves it as) exec must not panic.
func TestExecNilLogfIsSafe(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"run": "cid\n"}}
	e := engineWith(f) // logf nil
	if _, err := e.Start(context.Background(), RunSpec{Image: "img"}); err != nil {
		t.Fatalf("unexpected error with nil logf: %v", err)
	}
}

// TestSetLogfNilDisables verifies SetLogf(nil) turns logging back off rather than
// installing a nil function that exec would then call.
func TestSetLogfNilDisables(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"run": "cid\n"}}
	e := engineWith(f)

	var count int
	SetLogf(e, func(string, ...any) { count++ })
	SetLogf(e, nil)

	if _, err := e.Start(context.Background(), RunSpec{Image: "img"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if count != 0 {
		t.Errorf("logged %d times after SetLogf(nil), want 0", count)
	}
}

// TestSetLogfOnForeignEngineIsSafe verifies SetLogf ignores an Engine it does not
// know, rather than panicking. It takes the interface, so anything can be passed.
func TestSetLogfOnForeignEngineIsSafe(t *testing.T) {
	SetLogf(nil, func(string, ...any) {})
	SetLogf(otherEngine{}, func(string, ...any) {})
}

// otherEngine is an Engine that does not shell out, for the SetLogf guard above.
type otherEngine struct{}

func (otherEngine) Start(context.Context, RunSpec) (string, error) { return "", nil }
func (otherEngine) Stop(context.Context, string) error             { return nil }
func (otherEngine) Inspect(context.Context, string) (map[string][]PortBinding, error) {
	return nil, nil
}

// TestCommandLineQuoting covers the shell quoting. The logged line is meant to be
// pasteable, and Inspect really does pass a --format template whose braces and
// space a shell would mangle.
func TestCommandLineQuoting(t *testing.T) {
	tests := []struct {
		name string
		bin  string
		args []string
		want string
	}{
		{
			name: "plain arguments are not quoted",
			bin:  "docker",
			args: []string{"run", "-d", "img:latest"},
			want: "docker run -d img:latest",
		},
		{
			name: "a format template is quoted",
			bin:  "docker",
			args: []string{"inspect", "--format", "{{json .NetworkSettings.Ports}}", "cid"},
			want: `docker inspect --format '{{json .NetworkSettings.Ports}}' cid`,
		},
		{
			name: "a path with a space is quoted",
			bin:  "/usr/local/bin/podman",
			args: []string{"run", "-v", "/home/My Files/cfg.yaml:/tmp/config.yaml:ro", "img"},
			want: `/usr/local/bin/podman run -v '/home/My Files/cfg.yaml:/tmp/config.yaml:ro' img`,
		},
		{
			name: "an embedded single quote is spliced",
			bin:  "docker",
			args: []string{"run", "-e", "MSG=it's"},
			want: `docker run -e 'MSG=it'\''s'`,
		},
		{
			name: "an empty argument is visible",
			bin:  "docker",
			args: []string{"run", "-e", ""},
			want: "docker run -e ''",
		},
		{
			name: "common punctuation stays unquoted",
			bin:  "docker",
			args: []string{"run", "-p", "127.0.0.1:0:8080/tcp", "--name=a_b-c.d", "img@sha256:abc"},
			want: "docker run -p 127.0.0.1:0:8080/tcp --name=a_b-c.d img@sha256:abc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandLine(tc.bin, tc.args); got != tc.want {
				t.Errorf("commandLine() =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

// TestExecLogsPercentLiterally verifies an argument containing a % survives
// logging. The sink is Printf-style, so passing the line as a format string would
// turn a --format template's % into a bogus verb.
func TestExecLogsPercentLiterally(t *testing.T) {
	var got string
	e := &cliEngine{
		bin: "docker",
		run: func(_ context.Context, _ string, _ ...string) ([]byte, error) { return nil, nil },
		// Mirror cmd's sink, which formats before writing.
		logf: func(format string, args ...any) { got = fmt.Sprintf(format, args...) },
	}
	if _, err := e.exec(context.Background(), "inspect", "--format", "100%_{{.Id}}"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(got, "100%_") {
		t.Errorf("logged %q, want it to contain the literal %%", got)
	}
	if strings.Contains(got, "%!") {
		t.Errorf("logged %q, want no Printf verb errors", got)
	}
}

// TestStopRemovesContainer verifies Stop removes the container after stopping it.
// The run does not use --rm, so an exited container persists to hold its logs;
// removal here is what keeps that from leaking a dead container per run.
func TestStopRemovesContainer(t *testing.T) {
	f := &fakeRun{replies: map[string]string{
		"stop": "abc123\n",
		"rm":   "abc123\n",
	}}

	if err := engineWith(f).Stop(context.Background(), "abc123"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got, want := joined(f.argsFor(t, "rm")), "rm -f abc123"; got != want {
		t.Errorf("rm args = %q, want %q", got, want)
	}

	// Order matters: removing before stopping would kill the container without
	// giving it StopTimeout to shut down gracefully.
	var stopAt, rmAt = -1, -1
	for i, c := range f.calls {
		switch c[0] {
		case "stop":
			stopAt = i
		case "rm":
			rmAt = i
		}
	}
	if stopAt < 0 || rmAt < 0 {
		t.Fatalf("expected both a stop and an rm, got calls %v", f.calls)
	}
	if stopAt > rmAt {
		t.Errorf("rm ran before stop (calls %v); stop must come first", f.calls)
	}
}

// TestStopRemovesAfterFailedStop verifies the remove still happens when the stop
// fails. The likeliest reason for a stop to fail is that the container already
// exited — precisely the crashed-container case that leaves an artifact behind —
// so skipping the remove there would leak exactly what it must clean up.
func TestStopRemovesAfterFailedStop(t *testing.T) {
	f := &fakeRun{
		replies: map[string]string{
			"stop": "Error response from daemon: cannot stop container",
			"rm":   "abc123\n",
		},
		errs: map[string]error{"stop": errors.New("exit status 1")},
	}

	// The stop failure is still reported: it is the more meaningful error.
	if err := engineWith(f).Stop(context.Background(), "abc123"); err == nil {
		t.Error("expected the stop failure to be reported")
	}
	// But the container was removed anyway.
	if got, want := joined(f.argsFor(t, "rm")), "rm -f abc123"; got != want {
		t.Errorf("rm args = %q, want %q", got, want)
	}
}

// TestStopAbsentContainerSkipsRemove verifies that a container already gone needs
// no remove: there is nothing to clean up, so Stop returns without a second call.
func TestStopAbsentContainerSkipsRemove(t *testing.T) {
	f := &fakeRun{
		replies: map[string]string{"stop": "Error response from daemon: No such container: abc123"},
		errs:    map[string]error{"stop": errors.New("exit status 1")},
	}

	if err := engineWith(f).Stop(context.Background(), "abc123"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, c := range f.calls {
		if c[0] == "rm" {
			t.Errorf("removed an already-absent container: calls %v", f.calls)
		}
	}
}

// TestStopToleratesAbsentOnRemove verifies Stop succeeds when the container
// disappears between the stop and the remove. Something else reaping it achieves
// the same postcondition, so it is not a failure.
func TestStopToleratesAbsentOnRemove(t *testing.T) {
	for _, msg := range []string{
		"Error response from daemon: No such container: abc123",
		"Error: no container with name or ID \"abc123\" found",
	} {
		f := &fakeRun{
			replies: map[string]string{"stop": "abc123\n", "rm": msg},
			errs:    map[string]error{"rm": errors.New("exit status 1")},
		}
		if err := engineWith(f).Stop(context.Background(), "abc123"); err != nil {
			t.Errorf("Stop with rm reporting %q = %v, want nil", msg, err)
		}
	}
}

// TestStopReportsRemoveFailure verifies a real remove failure is not swallowed. A
// container that stopped but could not be removed is a leak the caller should
// hear about, since nothing else will clean it up.
func TestStopReportsRemoveFailure(t *testing.T) {
	f := &fakeRun{
		replies: map[string]string{
			"stop": "abc123\n",
			"rm":   "permission denied while trying to connect to the Docker daemon",
		},
		errs: map[string]error{"rm": errors.New("exit status 1")},
	}
	if err := engineWith(f).Stop(context.Background(), "abc123"); err == nil {
		t.Fatal("expected a real remove failure to be reported")
	}
}

// TestStartPortMappings verifies each fixed mapping becomes its own
// -p HOST:CONTAINER, in the order given.
//
// Distinct from the PublishPorts assertions above: that form publishes on an
// ephemeral host port, which cannot collide but also cannot be known in advance.
// A caller that must publish a specific host port — an OTLP receiver an SDK is
// configured to reach at 4318 — needs this form, and the two differ only in the
// value of the -p argument, so nothing but a test distinguishes them.
func TestStartPortMappings(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"run": "abc123\n"}}
	_, err := engineWith(f).Start(context.Background(), RunSpec{
		Image: "otel/opentelemetry-collector-contrib:latest",
		PortMappings: []PortMapping{
			{HostPort: 4317, ContainerPort: 4317},
			{HostPort: 14318, ContainerPort: 4318},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := joined(f.argsFor(t, "run"))
	for _, want := range []string{"-p 4317:4317", "-p 14318:4318"} {
		if !strings.Contains(got, want) {
			t.Errorf("run args %q missing %q", got, want)
		}
	}
	// A mapping must not degrade to the ephemeral form, which would publish on a
	// port the caller cannot predict.
	if strings.Contains(got, "-p 4317 ") {
		t.Errorf("run args %q published a bare port; the host side must be fixed", got)
	}
}

// TestStartPublishPortsAndMappingsCoexist verifies a spec may use both forms, with
// the ephemeral ports first.
func TestStartPublishPortsAndMappingsCoexist(t *testing.T) {
	f := &fakeRun{replies: map[string]string{"run": "abc123\n"}}
	_, err := engineWith(f).Start(context.Background(), RunSpec{
		Image:        "example/img:v1",
		PublishPorts: []int{9094},
		PortMappings: []PortMapping{{HostPort: 4318, ContainerPort: 4318}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := joined(f.argsFor(t, "run"))
	if !strings.Contains(got, "-p 9094") || !strings.Contains(got, "-p 4318:4318") {
		t.Errorf("run args %q should carry both the ephemeral and the fixed port", got)
	}
}
