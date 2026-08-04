// Package containers runs and inspects containers through a container CLI.
//
// It exists so callers that need to host something in a container — currently
// `rossoctl authbridge exec --proxyContainerImage` — can do so without knowing
// whether the local runtime is docker or podman, and can be tested without one
// being installed.
//
// Like the other internal packages it is free of Cobra. The command runner is
// injected via cliEngine.run, so tests can drive the engine without a real
// runtime on PATH.
package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// StopTimeout is how long the runtime is given to stop a container gracefully
// before it kills it, and also bounds the stop command itself.
const StopTimeout = 10 * time.Second

// runTimeout bounds the run and inspect commands. Starting a container is
// normally sub-second; a minute is generous enough to absorb an image pull on a
// slow link without hanging a CLI session indefinitely.
const runTimeout = time.Minute

// PortBinding is one host binding for a container port, as reported by
// `inspect --format '{{json .NetworkSettings.Ports}}'`.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// RunSpec describes a container to start.
type RunSpec struct {
	// Image is the container image reference. Required.
	Image string

	// Name, when set, is the container's name. Left empty the runtime assigns
	// one, and the returned ID is what identifies the container.
	Name string

	// PublishPorts are container ports to publish on an ephemeral host port —
	// the `-p PORT` form, which lets the kernel choose the host side. The
	// assigned ports are discovered afterwards with Inspect.
	PublishPorts []int

	// Mounts are host->container bind mounts.
	Mounts []Mount

	// Env are environment variables to set in the container, "K=V".
	Env []string

	// Args are the container's command arguments, appended after the image and
	// so passed to the image's own entrypoint.
	Args []string
}

// Mount is a bind mount of a host path into a container.
type Mount struct {
	// HostPath is the path on this host. Required, and must be absolute:
	// container CLIs treat a relative source as a named volume.
	HostPath string

	// ContainerPath is where it appears inside the container. Required.
	ContainerPath string

	// ReadOnly mounts it read-only, which is what a config or CA input wants:
	// the container has no reason to write back to the host.
	ReadOnly bool
}

// Engine starts, stops, and inspects containers.
//
// Implementations are safe for sequential use by one caller; they hold no state
// beyond how to invoke the runtime.
type Engine interface {
	// Start runs a container from spec, detached, and returns its ID. The
	// container is running when Start returns — but its entrypoint may still be
	// initializing, so a caller that needs readiness must wait for a signal of
	// its own (a published port answering, a file appearing).
	Start(ctx context.Context, spec RunSpec) (string, error)

	// Stop stops the container, giving it StopTimeout to exit before it is
	// killed. Stopping an already-stopped or absent container is not an error:
	// the goal is that it is not running, and it is not.
	Stop(ctx context.Context, id string) error

	// Inspect returns the container's published port bindings, keyed by the
	// container port and protocol as the runtime reports it ("8080/tcp"). A
	// container port that is published but not yet bound maps to a nil slice,
	// which is how the runtime reports it too.
	Inspect(ctx context.Context, id string) (map[string][]PortBinding, error)
}

// cliEngine implements Engine by invoking a container CLI — docker or podman,
// whose run/stop/inspect surfaces are compatible for what is used here.
type cliEngine struct {
	// bin is the runtime binary: an absolute path, or a name to be resolved on
	// PATH by exec.
	bin string

	// run executes a command and returns its combined output. It is a field so
	// tests can substitute a fake runtime; nil means really execute.
	run func(ctx context.Context, bin string, args ...string) ([]byte, error)
}

// NewCLIEngine returns an Engine backed by the container CLI named by bin
// (e.g. "docker", "podman", or an absolute path to one).
func NewCLIEngine(bin string) Engine {
	return &cliEngine{bin: bin}
}

// Detect returns an Engine for the local container runtime: $ROSSOCORTEX_RUNTIME
// if set and on PATH, else docker, else podman. It reports which binary was
// chosen so callers can name it in messages.
//
// Only PATH is checked, not whether the daemon is up — a runtime that is
// installed but not running fails at Start, with the runtime's own message,
// which says more than anything this could report up front.
func Detect() (engine Engine, bin string, err error) {
	var candidates []string
	if pref := os.Getenv("ROSSOCORTEX_RUNTIME"); pref != "" {
		candidates = append(candidates, pref)
	}
	candidates = append(candidates, "docker", "podman")

	for _, c := range candidates {
		if p, lookErr := exec.LookPath(c); lookErr == nil {
			return NewCLIEngine(p), c, nil
		}
	}
	return nil, "", fmt.Errorf("no container runtime found on PATH (looked for %s)",
		strings.Join(candidates, ", "))
}

// exec runs the runtime with args, returning its combined output. Combined
// rather than just stdout because a runtime's diagnostics go to stderr, and on
// failure that text is the only useful part of the error.
func (e *cliEngine) exec(ctx context.Context, args ...string) ([]byte, error) {
	if e.run != nil {
		return e.run(ctx, e.bin, args...)
	}
	out, err := exec.CommandContext(ctx, e.bin, args...).CombinedOutput()
	if err != nil {
		// The runtime's own message is what explains the failure (no such
		// image, daemon not running, port in use), so surface it rather than
		// just "exit status 125".
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return out, fmt.Errorf("%s %s: %w: %s", e.bin, strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("%s %s: %w", e.bin, strings.Join(args, " "), err)
	}
	return out, nil
}

// Start implements Engine.
func (e *cliEngine) Start(ctx context.Context, spec RunSpec) (string, error) {
	if spec.Image == "" {
		return "", fmt.Errorf("container image is required")
	}

	// --rm so a container that exits does not linger as a dead artifact the
	// operator has to clean up by hand; -d so Start returns rather than
	// blocking on the container's lifetime.
	args := []string{"run", "-d", "--rm"}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	for _, p := range spec.PublishPorts {
		// -p with a bare container port publishes it on an ephemeral host port,
		// which is the point: the host side is discovered with Inspect instead
		// of being hardcoded and risking a collision.
		args = append(args, "-p", strconv.Itoa(p))
	}
	for _, m := range spec.Mounts {
		args = append(args, "-v", m.arg())
	}
	for _, kv := range spec.Env {
		args = append(args, "-e", kv)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Args...)

	runCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	out, err := e.exec(runCtx, args...)
	if err != nil {
		return "", err
	}

	// `run -d` prints the container ID. Take the last non-empty line: an image
	// pull writes progress first, and with CombinedOutput that precedes the ID.
	id := lastLine(string(out))
	if id == "" {
		return "", fmt.Errorf("%s run: no container ID in output", e.bin)
	}
	return id, nil
}

// arg renders the mount as a -v value. The ro flag is a suffix on the same
// argument, which both docker and podman accept.
func (m Mount) arg() string {
	s := m.HostPath + ":" + m.ContainerPath
	if m.ReadOnly {
		s += ":ro"
	}
	return s
}

// Stop implements Engine.
func (e *cliEngine) Stop(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("container ID is required")
	}

	// Stop is a cleanup path, so it must not inherit a cancelled context: the
	// usual reason to stop a container is that the surrounding operation is
	// ending, and a cancelled parent would make the stop a no-op and leak the
	// container. Give it its own budget instead.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StopTimeout+runTimeout)
	defer cancel()

	seconds := strconv.Itoa(int(StopTimeout.Seconds()))
	out, err := e.exec(stopCtx, "stop", "-t", seconds, id)
	if err != nil {
		// A container that is already gone satisfies the postcondition. This is
		// the common case when the container ran with --rm and its process
		// exited on its own.
		if isNoSuchContainer(string(out)) {
			return nil
		}
		return err
	}
	return nil
}

// isNoSuchContainer reports whether a runtime's error output means the container
// does not exist. Both CLIs say so in prose, with no distinct exit code to test.
func isNoSuchContainer(out string) bool {
	s := strings.ToLower(out)
	return strings.Contains(s, "no such container") ||
		strings.Contains(s, "no container with name or id")
}

// Inspect implements Engine.
func (e *cliEngine) Inspect(ctx context.Context, id string) (map[string][]PortBinding, error) {
	if id == "" {
		return nil, fmt.Errorf("container ID is required")
	}

	inspectCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	out, err := e.exec(inspectCtx, "inspect", "--format", "{{json .NetworkSettings.Ports}}", id)
	if err != nil {
		return nil, err
	}

	// The template writes one line; an image pull cannot interleave here, but
	// podman may emit a warning first, so take the last line as with run.
	raw := lastLine(string(out))
	if raw == "" || raw == "null" {
		// No port map at all — a container with no published ports. Not an
		// error; the caller decides whether it needed one.
		return map[string][]PortBinding{}, nil
	}

	var ports map[string][]PortBinding
	if err := json.Unmarshal([]byte(raw), &ports); err != nil {
		return nil, fmt.Errorf("parsing %s inspect ports %q: %w", e.bin, raw, err)
	}
	return ports, nil
}

// HostPort returns the host port published for containerPort, preferring an
// IPv4 binding. Container CLIs commonly report both an IPv4 and an IPv6 binding
// for one published port; they carry the same host port, but the IPv4 one is
// what a client reliably reaches, so it is what gets reported.
func HostPort(ports map[string][]PortBinding, containerPort int) (int, bool) {
	key := strconv.Itoa(containerPort) + "/tcp"
	binds := ports[key]

	best := ""
	for _, b := range binds {
		if b.HostPort == "" {
			continue
		}
		// An IPv4 (or unspecified) host IP wins immediately; an IPv6-only
		// binding is kept as a fallback in case that is all there is.
		if !strings.Contains(b.HostIP, ":") {
			best = b.HostPort
			break
		}
		if best == "" {
			best = b.HostPort
		}
	}
	if best == "" {
		return 0, false
	}

	n, err := strconv.Atoi(best)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// lastLine returns the last non-empty, trimmed line of s, which is where a
// container CLI leaves the value it was asked for after any progress output.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if v := strings.TrimSpace(lines[i]); v != "" {
			return v
		}
	}
	return ""
}
