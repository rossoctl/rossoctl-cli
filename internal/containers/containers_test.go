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

// TestStartBuildsRunCommand verifies the run command carries the detached and
// auto-remove flags, one -p per published port, the mounts, and the container's
// own arguments after the image.
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
		Env:  []string{"LOG_LEVEL=debug"},
		Args: []string{"--config", "/tmp/config.yaml"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id != "abc123def4567890" {
		t.Errorf("id = %q, want the container ID", id)
	}

	got := joined(f.argsFor(t, "run"))
	for _, want := range []string{
		"run -d --rm",
		"-p 8080",
		"-p 9094",
		"-v /host/config.yaml:/tmp/config.yaml:ro",
		"-v /host/ca:/etc/ca",
		"-e LOG_LEVEL=debug",
		// The image must precede the container's own args, or they would be
		// parsed as flags of the runtime's.
		"example/proxy:v1 --config /tmp/config.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("run args %q missing %q", got, want)
		}
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
// not. A container run with --rm that exited on its own hits this every time.
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
