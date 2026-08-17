// Package otelcollect builds the inputs for a local OpenTelemetry collector that
// forwards traces to MLflow.
//
// It generates the collector's YAML configuration, decides where on this host
// that file can live so a container can bind-mount it, records what was
// generated, and reports whether MLflow is actually listening. Starting the
// container is the caller's job, through internal/containers.
//
// Like the other internal packages it is free of Cobra, and every path it writes
// to is derived from an injected base directory or the environment, so it can be
// tested against a temporary home.
package otelcollect

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultTracesEndpoint is where the generated config sends traces when
// --traces_endpoint is not given.
//
// host.containers.internal is the name podman gives the host from inside a
// container, and is what makes the default work unchanged: the collector runs in
// a container while MLflow runs on the host, so "localhost" in this value would
// name the collector itself. Docker resolves the same name on recent versions,
// and startContainer adds it as an --add-host entry regardless, so the default
// does not depend on which runtime is in use.
const DefaultTracesEndpoint = "http://host.containers.internal:5001/v1/traces"

// Image is the collector image that is run. The contrib distribution is required
// rather than incidental: otlphttp is in it and not in the core image.
const Image = "otel/opentelemetry-collector-contrib:latest"

// ContainerConfigPath is where the generated config is mounted inside the
// container. It is where the contrib image's entrypoint looks by default, so
// mounting over it needs no --config argument.
const ContainerConfigPath = "/etc/otelcol-contrib/config.yaml"

// OTLP ports the collector receives on. These are the OTLP defaults, and they
// are published on the same host port so an SDK pointed at localhost:4318 with no
// configuration finds the collector.
const (
	GRPCPort = 4317
	HTTPPort = 4318
)

// RecordName is the file, in the rossoctl config directory, recording what the
// last `otel collect` generated.
const RecordName = "otel-config.yaml"

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// Config is the generated collector configuration.
//
// It is a typed tree rather than a text template so the result is always
// well-formed YAML and the pieces the command varies are set as values. Field
// order in the emitted file follows this declaration; the reference config
// happens to be in alphabetical order, and keeping to it means a generated file
// diffs cleanly against a hand-written one.
//
// Only the exporter's traces endpoint varies today. The rest is fixed, so it is
// built by NewConfig rather than being reachable through flags — a knob nobody
// asked for is a knob that has to be tested and documented.
type Config struct {
	Exporters  Exporters  `yaml:"exporters"`
	Extensions Extensions `yaml:"extensions"`
	Processors Processors `yaml:"processors"`
	Receivers  Receivers  `yaml:"receivers"`
	Service    Service    `yaml:"service"`
}

// Exporters are where the pipeline sends spans: the collector's own log, and
// MLflow.
type Exporters struct {
	Debug  DebugExporter  `yaml:"debug"`
	MLflow MLflowExporter `yaml:"otlphttp/mlflow"`
}

// DebugExporter logs each span the collector receives. Kept in the pipeline
// because it is what distinguishes "the SDK never sent anything" from "MLflow
// rejected it" when a trace does not show up.
type DebugExporter struct {
	Verbosity string `yaml:"verbosity"`
}

// MLflowExporter posts spans to MLflow's OTLP traces endpoint.
type MLflowExporter struct {
	Headers        map[string]string `yaml:"headers"`
	RetryOnFailure RetryOnFailure    `yaml:"retry_on_failure"`
	SendingQueue   SendingQueue      `yaml:"sending_queue"`
	TLS            TLSConfig         `yaml:"tls"`

	// TracesEndpoint is the full URL of MLflow's traces collector, path
	// included. It is `traces_endpoint` rather than the more usual `endpoint`
	// because MLflow serves OTLP at /v1/traces under a prefix of its own, which
	// the signal-specific key sends to verbatim instead of appending its own path.
	TracesEndpoint string `yaml:"traces_endpoint"`
}

// RetryOnFailure re-sends a batch MLflow rejected or did not answer, so a restart
// of MLflow costs a delay rather than the spans buffered during it.
type RetryOnFailure struct {
	Enabled         bool   `yaml:"enabled"`
	InitialInterval string `yaml:"initial_interval"`
	MaxElapsedTime  string `yaml:"max_elapsed_time"`
	MaxInterval     string `yaml:"max_interval"`
}

// SendingQueue buffers batches so a slow MLflow does not block the receiver.
type SendingQueue struct {
	Enabled      bool `yaml:"enabled"`
	NumConsumers int  `yaml:"num_consumers"`
	QueueSize    int  `yaml:"queue_size"`
}

// TLSConfig carries the exporter's TLS settings. insecure is set because the
// default endpoint is plain HTTP to a local MLflow; it is the exporter's own
// switch, and has no effect on an https:// endpoint's certificate validation.
type TLSConfig struct {
	Insecure bool `yaml:"insecure"`
}

// Extensions are collector components outside the pipeline.
type Extensions struct {
	// HealthCheck serves a liveness endpoint. Empty struct, emitted as `{}`:
	// naming the extension with no settings is how the collector is told to load
	// it with its defaults.
	HealthCheck struct{} `yaml:"health_check"`
}

// Processors transform spans between receiver and exporter.
type Processors struct {
	// Batch groups spans before export, with defaults. Emitted as `{}` for the
	// same reason as health_check.
	Batch struct{} `yaml:"batch"`

	MemoryLimiter MemoryLimiter `yaml:"memory_limiter"`
}

// MemoryLimiter makes the collector shed load rather than grow without bound,
// which matters for a container that has no memory limit of its own.
type MemoryLimiter struct {
	CheckInterval string `yaml:"check_interval"`
	LimitMiB      int    `yaml:"limit_mib"`
}

// Receivers are where spans arrive.
type Receivers struct {
	OTLP OTLPReceiver `yaml:"otlp"`
}

// OTLPReceiver accepts OTLP over gRPC and HTTP.
type OTLPReceiver struct {
	Protocols OTLPProtocols `yaml:"protocols"`
}

// OTLPProtocols holds the two OTLP transports' listen addresses.
type OTLPProtocols struct {
	GRPC Endpoint `yaml:"grpc"`
	HTTP Endpoint `yaml:"http"`
}

// Endpoint is a listen address.
type Endpoint struct {
	Endpoint string `yaml:"endpoint"`
}

// Service wires the declared components into a running collector. A component
// defined above but not named here is not loaded.
type Service struct {
	Extensions []string            `yaml:"extensions"`
	Pipelines  map[string]Pipeline `yaml:"pipelines"`
}

// Pipeline is one receiver->processor->exporter path.
type Pipeline struct {
	Exporters  []string `yaml:"exporters"`
	Processors []string `yaml:"processors"`
	Receivers  []string `yaml:"receivers"`
}

// listenAddr is the address the receivers bind inside the container. 0.0.0.0
// rather than 127.0.0.1: a container's loopback is reachable only from inside it,
// so binding there would refuse every connection arriving through the published
// port.
const listenAddr = "0.0.0.0"

// NewConfig returns the collector configuration that forwards traces to
// tracesEndpoint.
//
// Every value other than the endpoint is fixed, and matches the reference
// configuration this was derived from.
func NewConfig(tracesEndpoint string) *Config {
	return &Config{
		Exporters: Exporters{
			Debug: DebugExporter{Verbosity: "detailed"},
			MLflow: MLflowExporter{
				// Experiment 0 is MLflow's own "Default" experiment, which always
				// exists — so traces land somewhere visible without the user having
				// to create an experiment first.
				Headers: map[string]string{"x-mlflow-experiment-id": "0"},
				RetryOnFailure: RetryOnFailure{
					Enabled:         true,
					InitialInterval: "5s",
					MaxElapsedTime:  "300s",
					MaxInterval:     "30s",
				},
				SendingQueue: SendingQueue{
					Enabled:      true,
					NumConsumers: 2,
					QueueSize:    1000,
				},
				TLS:            TLSConfig{Insecure: true},
				TracesEndpoint: tracesEndpoint,
			},
		},
		Processors: Processors{
			MemoryLimiter: MemoryLimiter{
				CheckInterval: "1s",
				LimitMiB:      1000,
			},
		},
		Receivers: Receivers{
			OTLP: OTLPReceiver{
				Protocols: OTLPProtocols{
					GRPC: Endpoint{Endpoint: listenAddr + ":" + strconv.Itoa(GRPCPort)},
					HTTP: Endpoint{Endpoint: listenAddr + ":" + strconv.Itoa(HTTPPort)},
				},
			},
		},
		Service: Service{
			Extensions: []string{"health_check"},
			Pipelines: map[string]Pipeline{
				"traces/mlflow": {
					Exporters:  []string{"debug", "otlphttp/mlflow"},
					Processors: []string{"memory_limiter", "batch"},
					Receivers:  []string{"otlp"},
				},
			},
		},
	}
}

// HTTPEndpoint returns the config's receivers.otlp.protocols.http.endpoint, which
// is the value the record file carries.
func (c *Config) HTTPEndpoint() string {
	return c.Receivers.OTLP.Protocols.HTTP.Endpoint
}

// Marshal renders the config as YAML.
func (c *Config) Marshal() ([]byte, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshaling collector config: %w", err)
	}
	return data, nil
}

// ConfigDir returns the directory the generated collector config is written to,
// ~/.config/rossoctl/otel.
//
// $XDG_CONFIG_HOME is honored, as it is by config.DefaultPath and
// instances.BaseDir, so everything rossoctl writes stays in one place — but only
// when it points inside the home directory. That condition is what the second
// return value reports, and it exists because this path has a constraint the
// other two do not: the file has to be bind-mountable into a container.
//
// A container runtime on macOS or Windows runs containers in a VM, and only the
// host directories that VM shares can be mounted. Both podman machine and Docker
// Desktop share the user's home directory by default and little else, so a config
// under an XDG_CONFIG_HOME pointing at, say, /etc/xdg would be written
// successfully and then fail to mount — as a container that starts and
// immediately exits, which says nothing about the cause. Falling back to the home
// directory keeps the mount working; the bool lets the caller say why it did.
func ConfigDir() (dir string, xdgIgnored bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("locating home directory: %w", err)
	}

	base := filepath.Join(home, ".config")
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		if withinHome(xdg, home) {
			base = xdg
		} else {
			xdgIgnored = true
		}
	}
	return filepath.Join(base, "rossoctl", "otel"), xdgIgnored, nil
}

// withinHome reports whether dir is home or below it, comparing cleaned absolute
// paths so ".." and a trailing slash cannot smuggle a path out of the tree.
//
// A relative XDG_CONFIG_HOME is resolved against the working directory, which is
// what the spec says to ignore entirely; treating it as outside home is the safe
// reading, since the caller then uses a path that is known to be mountable.
func withinHome(dir, home string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(home, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !hasDotDotPrefix(rel))
}

// hasDotDotPrefix reports whether a relative path's first element is "..", i.e.
// it escapes the base. Checked element-wise rather than as a string prefix so a
// sibling directory named "..config" is not mistaken for an escape.
func hasDotDotPrefix(rel string) bool {
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return first == ".."
}

// WriteConfig writes cfg into dir under a name derived from now, creating the
// directory, and returns the file's path.
//
// The name carries a timestamp rather than being fixed so a second
// `otel collect` does not rewrite the file a collector started by the first one
// is still using — the container holds a bind mount to this exact path, and
// rewriting it underneath would change a running collector's configuration on its
// next reload. The record file is what makes the current one findable.
func WriteConfig(dir string, cfg *Config, now time.Time) (string, error) {
	data, err := cfg.Marshal()
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}

	// UTC so two runs an hour apart around a DST boundary still sort in the order
	// they happened.
	path := filepath.Join(dir, "collector-"+now.UTC().Format("20060102-150405")+".yaml")
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

// Record is what is written to ~/.config/rossoctl/otel-config.yaml: the
// generated config's path and the endpoint its OTLP HTTP receiver binds.
//
// It exists so a later command, or a person, can find the configuration a running
// collector is using without having to guess which timestamped file it was.
type Record struct {
	// ConfigFile is the generated collector config on this host.
	ConfigFile string `yaml:"configFile"`

	// HTTPEndpoint is the config's receivers.otlp.protocols.http.endpoint, as
	// bound inside the container.
	HTTPEndpoint string `yaml:"httpEndpoint"`
}

// WriteRecord writes rec as YAML to path, creating the parent directory.
func WriteRecord(path string, rec Record) error {
	data, err := yaml.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", filepath.Base(path), err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.WriteFile(path, data, filePerm); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// EndpointPort returns the port named by a traces endpoint URL, supplying the
// scheme's default when the URL has none.
//
// Needed because the MLflow check and the message that suggests starting it both
// have to name a port, and the endpoint is the only place it is stated.
func EndpointPort(endpoint string) (int, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return 0, fmt.Errorf("parsing %q: %w", endpoint, err)
	}
	if u.Host == "" {
		return 0, fmt.Errorf("%q has no host", endpoint)
	}

	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return 0, fmt.Errorf("%q has an invalid port %q", endpoint, p)
		}
		return n, nil
	}

	switch u.Scheme {
	case "http":
		return 80, nil
	case "https":
		return 443, nil
	default:
		return 0, fmt.Errorf("%q has no port and scheme %q has no default", endpoint, u.Scheme)
	}
}

// dialTimeout bounds the MLflow reachability probe. The target is on this host,
// so a connection either completes in microseconds or is refused; the timeout
// only covers a firewall that blackholes the SYN, and is short because the
// outcome is a warning rather than a decision.
const dialTimeout = 500 * time.Millisecond

// Listening reports whether something accepts TCP connections on 127.0.0.1 at
// port.
//
// Deliberately loopback rather than the endpoint's own host. The endpoint names
// the host as the *container* reaches it (host.containers.internal), which does
// not resolve in this process; what can be checked here is whether the service is
// up on this machine, and MLflow started as suggested — bound to 0.0.0.0 — is
// reachable on loopback.
//
// A successful dial is not proof it is MLflow, only that the port is taken. That
// is enough for what this drives: a warning that is skipped when something is
// there.
func Listening(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), dialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// DefaultOTLPTracesURL is where send-mock-trace posts, the OTLP/HTTP traces path
// on the port `otel collect` publishes.
//
// localhost, not the container-facing host.containers.internal used by the
// exporter's endpoint: this request is made by rossoctl on the host, to the port
// the collector publishes there.
const DefaultOTLPTracesURL = "http://localhost:" + otlpHTTPPortString + "/v1/traces"

// otlpHTTPPortString is HTTPPort as a string, so DefaultOTLPTracesURL can be a
// constant expression rather than being built at init time. A test asserts the two
// agree, since nothing else would notice them drifting apart.
const otlpHTTPPortString = "4318"

// TraceIDLen and SpanIDLen are the sizes, in bytes, of the two OTLP identifiers:
// 16 and 8 bytes, rendered as 32 and 16 hex characters in OTLP/JSON.
const (
	TraceIDLen = 16
	SpanIDLen  = 8
)

// MockSpanName is the name given to the generated span. Fixed, and recognizable on
// sight in a trace viewer, because the span exists to prove the path works.
const MockSpanName = "rossoctl-mock-span"

// spanKindInternal is SPAN_KIND_INTERNAL, the OTLP enum value for a span that has
// no remote parent or child. Correct for a span that models nothing.
const spanKindInternal = 1

// mockSpanDuration is how long the generated span claims to have taken: its start
// time is this much before its end time.
const mockSpanDuration = time.Second

// TracePayload is an OTLP/HTTP traces request body.
//
// A typed tree rather than a formatted string so the JSON is always well-formed,
// and shaped to OTLP/JSON's own encoding rules, which differ from the protobuf in
// ways that matter here: the two ID fields are hex strings rather than byte
// arrays, and the nanosecond timestamps are *strings*, because they exceed the
// range a JSON number is safely parsed into.
type TracePayload struct {
	ResourceSpans []ResourceSpans `json:"resourceSpans"`
}

// ResourceSpans are the spans produced by one resource — here, one service.
type ResourceSpans struct {
	Resource   Resource     `json:"resource"`
	ScopeSpans []ScopeSpans `json:"scopeSpans"`
}

// Resource describes what produced the spans, as a list of attributes.
type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

// KeyValue is one resource attribute. OTLP wraps every value in a single-key
// object naming its type, which is why Value is a struct rather than a string.
type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// AnyValue is an OTLP attribute value. Only the string form is needed here.
type AnyValue struct {
	StringValue string `json:"stringValue"`
}

// ScopeSpans are spans from one instrumentation scope. The scope itself is omitted:
// it is optional, and there is no library to name.
type ScopeSpans struct {
	Spans []Span `json:"spans"`
}

// Span is one OTLP span.
type Span struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
	Name    string `json:"name"`
	Kind    int    `json:"kind"`

	// Strings, not numbers: a nanosecond Unix timestamp needs more than the 53
	// bits a JSON number is guaranteed to carry, and OTLP/JSON specifies the
	// string form for 64-bit integers. Sent as a number, a receiver may parse it
	// through a float64 and shift the timestamp.
	StartTimeUnixNano string `json:"startTimeUnixNano"`
	EndTimeUnixNano   string `json:"endTimeUnixNano"`
}

// randomHex returns n random bytes as a lowercase hex string, retrying until the
// value is not all zeros.
//
// The retry is not paranoia about the generator: OTLP defines an all-zero trace or
// span ID as *invalid*, and a collector is entitled to reject the span. The odds
// are negligible (2^-64 for a span ID) but the failure would be a silently dropped
// trace, so it is cheaper to exclude than to explain.
func randomHex(n int) (string, error) {
	for range 8 {
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generating %d random bytes: %w", n, err)
		}
		if !allZero(b) {
			return hex.EncodeToString(b), nil
		}
	}
	// Eight all-zero draws in a row means the generator is broken, not unlucky.
	return "", fmt.Errorf("random source returned only zero bytes")
}

// allZero reports whether every byte of b is zero.
func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// NewMockTrace builds a single-span trace for serviceName, ending at end and
// starting one second before it, with a fresh random trace and span ID.
//
// end is passed in rather than read from the clock so the payload is reproducible
// under test.
func NewMockTrace(serviceName string, end time.Time) (*TracePayload, error) {
	traceID, err := randomHex(TraceIDLen)
	if err != nil {
		return nil, err
	}
	spanID, err := randomHex(SpanIDLen)
	if err != nil {
		return nil, err
	}

	start := end.Add(-mockSpanDuration)
	return &TracePayload{
		ResourceSpans: []ResourceSpans{{
			Resource: Resource{
				Attributes: []KeyValue{{
					// service.name is the conventional attribute every trace
					// backend groups and filters by, so it is what makes the span
					// findable in MLflow.
					Key:   "service.name",
					Value: AnyValue{StringValue: serviceName},
				}},
			},
			ScopeSpans: []ScopeSpans{{
				Spans: []Span{{
					TraceID:           traceID,
					SpanID:            spanID,
					Name:              MockSpanName,
					Kind:              spanKindInternal,
					StartTimeUnixNano: strconv.FormatInt(start.UnixNano(), 10),
					EndTimeUnixNano:   strconv.FormatInt(end.UnixNano(), 10),
				}},
			}},
		}},
	}, nil
}

// TraceID returns the payload's trace ID, so a caller can report what it sent.
// Empty if the payload has no span, which NewMockTrace never produces.
func (p *TracePayload) TraceID() string {
	if len(p.ResourceSpans) == 0 || len(p.ResourceSpans[0].ScopeSpans) == 0 ||
		len(p.ResourceSpans[0].ScopeSpans[0].Spans) == 0 {
		return ""
	}
	return p.ResourceSpans[0].ScopeSpans[0].Spans[0].TraceID
}

// SpanID returns the payload's span ID, on the same terms as TraceID.
func (p *TracePayload) SpanID() string {
	if len(p.ResourceSpans) == 0 || len(p.ResourceSpans[0].ScopeSpans) == 0 ||
		len(p.ResourceSpans[0].ScopeSpans[0].Spans) == 0 {
		return ""
	}
	return p.ResourceSpans[0].ScopeSpans[0].Spans[0].SpanID
}

// sendTimeout bounds the trace POST. The collector is local, so this covers a
// process that is listening but wedged rather than any real network latency.
const sendTimeout = 10 * time.Second

// PartialSuccess is the OTLP partial-success report: a 200 response may still say
// that some spans were dropped, which is otherwise indistinguishable from success.
type PartialSuccess struct {
	RejectedSpans int64  `json:"rejectedSpans,string"`
	ErrorMessage  string `json:"errorMessage"`
}

// traceResponse is the OTLP/HTTP ExportTraceServiceResponse.
type traceResponse struct {
	PartialSuccess PartialSuccess `json:"partialSuccess"`
}

// SendTrace posts payload to url as OTLP/HTTP JSON.
//
// It returns the partial-success report when the collector supplied one. A 200
// with rejectedSpans set is the case worth surfacing: the request succeeded but
// the span did not land, and reporting only the status code would call that a
// success.
//
// client may be nil, in which case one with sendTimeout is used.
func SendTrace(ctx context.Context, client *http.Client, url string, payload *TracePayload) (*PartialSuccess, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding trace payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	if client == nil {
		client = &http.Client{Timeout: sendTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		// A refused connection is the expected failure — the collector is not
		// running — so the error has to carry the URL that was tried.
		return nil, fmt.Errorf("posting trace to %s: %w", url, err)
	}
	defer resp.Body.Close()

	// Read the body whatever the status: on failure it carries the collector's
	// explanation, and on success it may carry a partial-success report.
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if msg := strings.TrimSpace(string(respBody)); msg != "" {
			return nil, fmt.Errorf("posting trace to %s: HTTP %d: %s", url, resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("posting trace to %s: HTTP %d", url, resp.StatusCode)
	}
	if readErr != nil {
		return nil, fmt.Errorf("reading the response from %s: %w", url, readErr)
	}

	// An empty body is a valid success response, and so is one that is not JSON at
	// all from something that is not a collector; neither is worth failing over
	// once the status says the span was accepted.
	var decoded traceResponse
	if len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, &decoded); err != nil {
			return nil, nil
		}
	}
	if decoded.PartialSuccess.RejectedSpans > 0 || decoded.PartialSuccess.ErrorMessage != "" {
		return &decoded.PartialSuccess, nil
	}
	return nil, nil
}

// PortsInUse returns which of the OTLP ports already have a listener on this
// host, in the order given.
//
// Checked up front because the OTLP ports are published on fixed host ports,
// which a runtime refuses to bind when they are taken — and it refuses with
// "address already in use" naming a port number, which does not say that the
// likely cause is a collector from an earlier run still holding it. The most
// common way to reach this is running this command twice.
func PortsInUse(ports ...int) []int {
	var taken []int
	for _, p := range ports {
		if Listening(p) {
			taken = append(taken, p)
		}
	}
	return taken
}

// MLflowHint is the warning shown when nothing is listening on the traces
// endpoint's port: the command that starts MLflow so it can receive what the
// collector will forward.
//
// --host 0.0.0.0 rather than the default loopback bind, because the collector
// reaches MLflow from inside a container, where a loopback-bound server on the
// host is unreachable. --allowed-hosts '*' for the matching reason: MLflow rejects
// a request whose Host header it does not recognize, and the collector's requests
// carry host.containers.internal.
func MLflowHint(port int) string {
	return fmt.Sprintf("mlflow server --host 0.0.0.0 --port %d --allowed-hosts '*'", port)
}
