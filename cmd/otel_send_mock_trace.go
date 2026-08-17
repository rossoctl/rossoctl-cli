package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/otelcollect"
)

// Bound to `otel send-mock-trace`'s flags.
var (
	otelSendServiceName string
	otelSendURL         string
)

// defaultMockServiceName is the service.name attribute the generated span carries
// when --serviceName is not given.
const defaultMockServiceName = "rossoctl-cli"

var otelSendMockTraceCmd = &cobra.Command{
	Use:   "send-mock-trace",
	Short: "Send a single mock trace to a local OpenTelemetry collector",
	Long: `Send a single mock trace to a local OpenTelemetry collector.

Posts one span, as OTLP/HTTP JSON, to ` + otelcollect.DefaultOTLPTracesURL + `.
The trace and span IDs are random, so each run produces a distinct trace. The span
is named ` + otelcollect.MockSpanName + `, ends now, and started one second ago.

Its resource carries one attribute, service.name, set from --serviceName. That is
the attribute a trace backend groups by, so it is what the span is found under in
MLflow.

Use this to check the path an agent's spans take — collector reachable, pipeline
configured, MLflow accepting — without having to run an instrumented workload.
"rossoctl otel collect" starts the collector this sends to.`,
	Args: cobra.NoArgs,
	RunE: runOtelSendMockTrace,
}

func runOtelSendMockTrace(cmd *cobra.Command, _ []string) error {
	if otelSendServiceName == "" {
		// An empty service.name is accepted by the collector and then groups the
		// span under nothing, which is a confusing way to discover the flag was
		// passed an empty value.
		return fmt.Errorf("--serviceName must not be empty")
	}

	// Current time for the end, one second earlier for the start. Read once so the
	// two are exactly a second apart rather than a second plus the time it took to
	// build the payload.
	payload, err := otelcollect.NewMockTrace(otelSendServiceName, timeNowOtel())
	if err != nil {
		return err
	}

	if verbose {
		fmt.Fprintf(cmd.ErrOrStderr(), "POST %s (service.name %q)\n", otelSendURL, otelSendServiceName)
	}

	partial, err := otelcollect.SendTrace(cmd.Context(), nil, otelSendURL, payload)
	if err != nil {
		// The overwhelmingly likely cause is that no collector is running, and the
		// fix is a command away, so name it rather than leaving a bare dial error.
		return fmt.Errorf("%w\nIs a collector listening? Start one with `rossoctl otel collect`", err)
	}

	// A 200 that reports rejected spans is not a success: the request was accepted
	// and the span was not. Reported as an error so a script checking the exit
	// status is not told the trace landed when it did not.
	if partial != nil {
		return fmt.Errorf("the collector rejected %d span(s): %s",
			partial.RejectedSpans, partial.ErrorMessage)
	}

	cmd.Printf("Sent trace %s (span %s) for service %q to %s\n",
		payload.TraceID(), payload.SpanID(), otelSendServiceName, otelSendURL)
	return nil
}

func init() {
	f := otelSendMockTraceCmd.Flags()
	f.StringVar(&otelSendServiceName, "serviceName", defaultMockServiceName,
		"value of the span's service.name resource attribute")
	// Offered because the collector's port is a published host port that can be
	// remapped, and because a mock trace is a natural way to probe a collector
	// somewhere other than this host.
	f.StringVar(&otelSendURL, "url", otelcollect.DefaultOTLPTracesURL,
		"OTLP/HTTP traces endpoint to post to")
}
