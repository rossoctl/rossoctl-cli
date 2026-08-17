package cmd

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/containers"
	"github.com/rossoctl/rossoctl-cli/internal/otelcollect"
)

// otelCollectTracesEndpoint backs --traces_endpoint.
//
// Named with underscores, against this package's other flags, to match the
// collector configuration key it sets: the value is copied verbatim into the
// otlphttp/mlflow exporter's traces_endpoint, and a user comparing the two reads
// one name rather than translating between them.
var otelCollectTracesEndpoint string

// timeNowOtel is indirected so a test can pin the timestamp in the generated
// config's filename. Separate from the timeNow in agents_authbridge_set.go, which
// a wait loop also stubs; sharing one would make the two tests interfere.
var timeNowOtel = time.Now

var otelCollectCmd = &cobra.Command{
	Use:   "collect",
	Short: "Run a local OpenTelemetry collector that forwards traces to MLflow",
	Long: `Run a local OpenTelemetry collector that forwards traces to MLflow.

Generates a collector configuration, writes it under ~/.config/rossoctl/otel, and
starts ` + otelcollect.Image + ` with that file
mounted at ` + otelcollect.ContainerConfigPath + `. The collector receives OTLP on
4317 (gRPC) and 4318 (HTTP), both published on the same host port, so an SDK
pointed at localhost:4318 needs no further configuration.

Spans are forwarded to the MLflow traces endpoint named by --traces_endpoint. That
default reaches the host from inside the container, where "localhost" would mean
the collector itself. MLflow has to be listening for the spans to arrive; this
command warns, and still starts the collector, when nothing is.

The generated file's path and the receiver's HTTP endpoint are recorded in
~/.config/rossoctl/` + otelcollect.RecordName + `.

The container is started detached and is not removed on exit. Stop it with
"docker stop" or "podman stop" using the name printed on start.

This needs docker or podman on PATH ($ROSSOCORTEX_RUNTIME overrides which).`,
	Args: cobra.NoArgs,
	RunE: runOtelCollect,
}

func runOtelCollect(cmd *cobra.Command, _ []string) error {
	errOut := cmd.ErrOrStderr()

	// Parse the endpoint before anything is written or started: it is the one
	// input that can be wrong, and it decides the port to probe.
	port, err := otelcollect.EndpointPort(otelCollectTracesEndpoint)
	if err != nil {
		return fmt.Errorf("--traces_endpoint: %w", err)
	}

	// Warn rather than fail. MLflow can be started after the collector, and the
	// exporter's retry_on_failure means spans buffered in the meantime are still
	// delivered — so a missing MLflow is a thing to say, not a reason to refuse.
	if !otelcollect.Listening(port) {
		fmt.Fprintf(errOut,
			"warning: nothing is listening on 127.0.0.1:%d, so traces will not reach MLflow.\nStart it with:\n\n  %s\n\n",
			port, otelcollect.MLflowHint(port))
	}

	// Refuse early when the OTLP ports are taken. The runtime would fail at `run`
	// with "address already in use" and a port number, having already written a
	// config file for a collector that never started; the likeliest cause is a
	// collector from an earlier run, which is worth saying rather than leaving to
	// be worked out.
	if taken := otelcollect.PortsInUse(otelcollect.GRPCPort, otelcollect.HTTPPort); len(taken) > 0 {
		return fmt.Errorf("port %s already in use on this host, so the collector cannot publish %d and %d;\n"+
			"stop whatever holds them — a collector from an earlier run would appear in `%s ps` — and try again",
			joinPorts(taken), otelcollect.GRPCPort, otelcollect.HTTPPort, runtimeNameForHint())
	}

	// One timestamp for the whole run, so the config filename and the container
	// name agree and the name printed at the end is the one that was started.
	// Calling timeNowOtel() at each use would let the clock tick between them.
	now := timeNowOtel()
	containerName := otelCollectContainerName(now)

	cfg := otelcollect.NewConfig(otelCollectTracesEndpoint)

	dir, xdgIgnored, err := otelcollect.ConfigDir()
	if err != nil {
		return err
	}
	if xdgIgnored {
		// Said out loud because the file is not where XDG_CONFIG_HOME says it
		// should be, and a user who set that variable deliberately deserves to
		// know which path won and why.
		fmt.Fprintf(errOut,
			"warning: ignoring XDG_CONFIG_HOME, which points outside the home directory;\n"+
				"a container can only bind-mount paths the runtime shares, so writing to %s instead\n", dir)
	}

	cfgPath, err := otelcollect.WriteConfig(dir, cfg, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(errOut, "wrote collector config %s\n", cfgPath)

	engine, bin, err := containers.Detect()
	if err != nil {
		return err
	}
	if verbose {
		fmt.Fprintf(errOut, "using container runtime %s\n", bin)
		containers.SetLogf(engine, func(format string, args ...any) {
			fmt.Fprintf(errOut, format+"\n", args...)
		})
	}

	id, err := engine.Start(cmd.Context(), containers.RunSpec{
		Image: otelcollect.Image,
		Name:  containerName,
		// Fixed host ports, not the ephemeral PublishPorts used elsewhere: an SDK
		// is configured with the OTLP port up front and cannot discover one the
		// kernel chose.
		PortMappings: []containers.PortMapping{
			{HostPort: otelcollect.GRPCPort, ContainerPort: otelcollect.GRPCPort},
			{HostPort: otelcollect.HTTPPort, ContainerPort: otelcollect.HTTPPort},
		},
		Mounts: []containers.Mount{{
			HostPath:      cfgPath,
			ContainerPath: otelcollect.ContainerConfigPath,
			// Read-only: the collector reads its configuration and has no reason
			// to write back to the host.
			ReadOnly: true,
		}},
		// host.containers.internal is podman's name for the host and is what the
		// default endpoint uses. Added explicitly so the same default also
		// resolves under docker, where the name is not always present.
		HostEntries: []containers.HostEntry{{
			Name:    "host.containers.internal",
			Address: containers.HostGateway,
		}},
	})
	if err != nil {
		return err
	}

	// The record is written after the container starts, so what it names is a
	// collector that is actually running.
	recordPath, err := otelCollectRecordPath()
	if err != nil {
		return err
	}
	if err := otelcollect.WriteRecord(recordPath, otelcollect.Record{
		ConfigFile:   cfgPath,
		HTTPEndpoint: cfg.HTTPEndpoint(),
	}); err != nil {
		return err
	}

	cmd.Printf("OpenTelemetry collector %s started, forwarding traces to %s\n",
		containerName, otelCollectTracesEndpoint)
	cmd.Printf("receiving OTLP on 127.0.0.1:%d (gRPC) and 127.0.0.1:%d (HTTP)\n",
		otelcollect.GRPCPort, otelcollect.HTTPPort)
	cmd.Printf("stop it with: %s stop %s\n", bin, containerName)

	if verbose {
		fmt.Fprintf(errOut, "container ID %s\n", id)
		fmt.Fprintf(errOut, "wrote %s\n", recordPath)
	}
	return nil
}

// joinPorts renders a port list for the in-use message, as "4317" or
// "4317 and 4318" — the message reads as prose, so a bare comma-join would not fit
// it.
func joinPorts(ports []int) string {
	switch len(ports) {
	case 0:
		return ""
	case 1:
		return strconv.Itoa(ports[0])
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// runtimeNameForHint returns the container CLI to name in the port-in-use hint,
// falling back to "docker" when none is installed.
//
// The fallback matters because this hint is produced before the runtime is
// detected — the ports are checked first so nothing is written when they are
// taken — and a missing runtime is a separate failure that Detect reports later,
// with its own message. Suggesting a plausible command beats saying nothing.
func runtimeNameForHint() string {
	if _, bin, err := containers.Detect(); err == nil {
		return bin
	}
	return "docker"
}

// otelCollectContainerName is the name given to the started container.
//
// Named rather than left to the runtime so the stop instruction can name
// something stable, and timestamped so a second collector — one started against a
// different endpoint — does not collide with the first.
func otelCollectContainerName(now time.Time) string {
	return "rossoctl-otelcol-" + now.UTC().Format("20060102-150405")
}

// otelCollectRecordPath returns ~/.config/rossoctl/otel-config.yaml.
//
// Derived from config.DefaultPath rather than rebuilt so the record lands beside
// config.yaml whatever that resolves to, including under an XDG_CONFIG_HOME. This
// file is only read from this host, so it has none of the mountability constraint
// that makes ConfigDir treat XDG_CONFIG_HOME differently.
func otelCollectRecordPath() (string, error) {
	cfgPath, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), otelcollect.RecordName), nil
}

func init() {
	otelCollectCmd.Flags().StringVar(&otelCollectTracesEndpoint, "traces_endpoint",
		otelcollect.DefaultTracesEndpoint,
		"MLflow OTLP traces endpoint the collector forwards spans to")
}
