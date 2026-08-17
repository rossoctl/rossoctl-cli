package cmd

var otelCmd = newGroup("otel", "Work with OpenTelemetry collection")

func init() {
	otelCmd.Long = `Work with OpenTelemetry collection.

"otel collect" runs a local OpenTelemetry collector in a container, receiving OTLP
from anything on this host and forwarding traces to MLflow, so an agent's spans
can be inspected without a hosted trace backend.

"otel send-mock-trace" posts one span to that collector, which checks the path from
here to MLflow without an instrumented workload to produce real spans.`

	otelCmd.AddCommand(otelCollectCmd, otelSendMockTraceCmd)
	rootCmd.AddCommand(otelCmd)
}
