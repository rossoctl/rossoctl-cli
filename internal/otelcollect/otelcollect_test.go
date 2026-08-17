package otelcollect

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// decode renders a config to YAML and reads it back as a generic tree, which is
// how these tests assert on what is actually written rather than on the Go
// struct: the YAML keys are the collector's interface, and a wrong tag would be
// invisible to a struct comparison.
func decode(t *testing.T, cfg *Config) map[string]any {
	t.Helper()
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshaling generated config: %v\n%s", err, data)
	}
	return got
}

// dig walks a decoded YAML tree by key, failing the test if the path is absent.
func dig(t *testing.T, tree map[string]any, path ...string) any {
	t.Helper()
	var cur any = tree
	for i, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s is not a mapping (at %q)", strings.Join(path[:i], "."), key)
		}
		cur, ok = m[key]
		if !ok {
			t.Fatalf("%s is missing from the generated config", strings.Join(path[:i+1], "."))
		}
	}
	return cur
}

// TestNewConfigTracesEndpoint verifies the flag's value lands on the exporter key
// it is named after, which is the command's whole purpose.
func TestNewConfigTracesEndpoint(t *testing.T) {
	const endpoint = "http://192.168.1.5:5002/v1/traces"
	got := dig(t, decode(t, NewConfig(endpoint)), "exporters", "otlphttp/mlflow", "traces_endpoint")
	if got != endpoint {
		t.Errorf("traces_endpoint = %v, want %q", got, endpoint)
	}
}

// TestNewConfigMatchesReference verifies the generated config against the
// reference it was derived from, key by key.
//
// This is the test that would catch a silent change to a value the collector
// depends on — a retry interval, the queue size, the memory limit — none of which
// any other assertion here covers. Written as a flat path->value table so a
// mismatch names the exact key.
func TestNewConfigMatchesReference(t *testing.T) {
	tree := decode(t, NewConfig(DefaultTracesEndpoint))

	for _, tc := range []struct {
		path []string
		want any
	}{
		{[]string{"exporters", "debug", "verbosity"}, "detailed"},
		{[]string{"exporters", "otlphttp/mlflow", "headers", "x-mlflow-experiment-id"}, "0"},
		{[]string{"exporters", "otlphttp/mlflow", "retry_on_failure", "enabled"}, true},
		{[]string{"exporters", "otlphttp/mlflow", "retry_on_failure", "initial_interval"}, "5s"},
		{[]string{"exporters", "otlphttp/mlflow", "retry_on_failure", "max_elapsed_time"}, "300s"},
		{[]string{"exporters", "otlphttp/mlflow", "retry_on_failure", "max_interval"}, "30s"},
		{[]string{"exporters", "otlphttp/mlflow", "sending_queue", "enabled"}, true},
		{[]string{"exporters", "otlphttp/mlflow", "sending_queue", "num_consumers"}, 2},
		{[]string{"exporters", "otlphttp/mlflow", "sending_queue", "queue_size"}, 1000},
		{[]string{"exporters", "otlphttp/mlflow", "tls", "insecure"}, true},
		{[]string{"exporters", "otlphttp/mlflow", "traces_endpoint"}, DefaultTracesEndpoint},
		{[]string{"processors", "memory_limiter", "check_interval"}, "1s"},
		{[]string{"processors", "memory_limiter", "limit_mib"}, 1000},
		{[]string{"receivers", "otlp", "protocols", "grpc", "endpoint"}, "0.0.0.0:4317"},
		{[]string{"receivers", "otlp", "protocols", "http", "endpoint"}, "0.0.0.0:4318"},
	} {
		t.Run(strings.Join(tc.path, "."), func(t *testing.T) {
			if got := dig(t, tree, tc.path...); got != tc.want {
				t.Errorf("%s = %#v, want %#v", strings.Join(tc.path, "."), got, tc.want)
			}
		})
	}

	// The two settings-free components must be present as empty mappings. A
	// missing key means the component is not loaded at all, and yaml renders the
	// empty struct as `{}`, which decodes to an empty map.
	for _, path := range [][]string{{"extensions", "health_check"}, {"processors", "batch"}} {
		got := dig(t, tree, path...)
		if m, ok := got.(map[string]any); !ok || len(m) != 0 {
			t.Errorf("%s = %#v, want an empty mapping", strings.Join(path, "."), got)
		}
	}
}

// TestNewConfigServicePipeline verifies the pipeline names the components it
// needs, in order.
//
// Worth asserting separately: a component defined in the config but absent from
// service.pipelines is silently not loaded, so a collector could start clean and
// forward nothing at all.
func TestNewConfigServicePipeline(t *testing.T) {
	tree := decode(t, NewConfig(DefaultTracesEndpoint))

	for _, tc := range []struct {
		path []string
		want []string
	}{
		{[]string{"service", "extensions"}, []string{"health_check"}},
		{[]string{"service", "pipelines", "traces/mlflow", "exporters"}, []string{"debug", "otlphttp/mlflow"}},
		{[]string{"service", "pipelines", "traces/mlflow", "processors"}, []string{"memory_limiter", "batch"}},
		{[]string{"service", "pipelines", "traces/mlflow", "receivers"}, []string{"otlp"}},
	} {
		t.Run(strings.Join(tc.path, "."), func(t *testing.T) {
			raw, ok := dig(t, tree, tc.path...).([]any)
			if !ok {
				t.Fatalf("%s is not a sequence", strings.Join(tc.path, "."))
			}
			if len(raw) != len(tc.want) {
				t.Fatalf("%s = %#v, want %#v", strings.Join(tc.path, "."), raw, tc.want)
			}
			for i, w := range tc.want {
				if raw[i] != w {
					t.Errorf("%s[%d] = %v, want %v", strings.Join(tc.path, "."), i, raw[i], w)
				}
			}
		})
	}
}

// TestHTTPEndpoint verifies the accessor reports the receiver endpoint the record
// file carries, rather than the gRPC one beside it.
func TestHTTPEndpoint(t *testing.T) {
	if got := NewConfig(DefaultTracesEndpoint).HTTPEndpoint(); got != "0.0.0.0:4318" {
		t.Errorf("HTTPEndpoint() = %q, want 0.0.0.0:4318", got)
	}
}

func TestEndpointPort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		want     int
	}{
		{"explicit port", "http://host.containers.internal:5001/v1/traces", 5001},
		{"the default endpoint", DefaultTracesEndpoint, 5001},
		{"http default", "http://example.com/v1/traces", 80},
		{"https default", "https://example.com/v1/traces", 443},
		{"loopback", "http://127.0.0.1:8080/v1/traces", 8080},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EndpointPort(tc.endpoint)
			if err != nil {
				t.Fatalf("EndpointPort(%q): %v", tc.endpoint, err)
			}
			if got != tc.want {
				t.Errorf("EndpointPort(%q) = %d, want %d", tc.endpoint, got, tc.want)
			}
		})
	}
}

func TestEndpointPortErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
	}{
		// A bare host:port parses as a URL whose scheme is the host, so it has no
		// Host at all — worth rejecting explicitly, since it is a plausible thing
		// to type for a flag whose default is a URL.
		{"no scheme", "host.containers.internal:5001"},
		{"empty", ""},
		{"scheme with no default port", "ftp://example.com/v1/traces"},
		{"non-numeric port", "http://example.com:notaport/v1/traces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EndpointPort(tc.endpoint); err == nil {
				t.Errorf("EndpointPort(%q) succeeded; want an error", tc.endpoint)
			}
		})
	}
}

// TestConfigDirUsesXDGWithinHome verifies XDG_CONFIG_HOME is honored when it
// points inside the home directory, matching the other rossoctl paths.
func TestConfigDirUsesXDGWithinHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := filepath.Join(home, "myconfig")
	t.Setenv("XDG_CONFIG_HOME", xdg)

	dir, ignored, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if ignored {
		t.Error("XDG_CONFIG_HOME inside home should be honored, not ignored")
	}
	if want := filepath.Join(xdg, "rossoctl", "otel"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}

// TestConfigDirIgnoresXDGOutsideHome verifies an XDG_CONFIG_HOME outside the home
// directory is reported and not used.
//
// The reason is mountability, not taste: a container runtime on macOS or Windows
// can only bind-mount the host paths its VM shares, which by default is the home
// directory. Honoring an /etc/xdg here would write the file successfully and then
// fail at `run`.
func TestConfigDirIgnoresXDGOutsideHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, tc := range []struct {
		name string
		xdg  string
	}{
		{"absolute elsewhere", filepath.Join(t.TempDir(), "elsewhere")},
		// Escapes home by traversal, which a plain prefix test would accept.
		{"traversal out of home", filepath.Join(home, "..", "outside")},
		// A relative value is unspecified by XDG; treated as outside so the
		// resulting path is one that is known to be mountable.
		{"relative", "relative/config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", tc.xdg)

			dir, ignored, err := ConfigDir()
			if err != nil {
				t.Fatalf("ConfigDir: %v", err)
			}
			if !ignored {
				t.Errorf("XDG_CONFIG_HOME %q is outside %q and should be reported as ignored", tc.xdg, home)
			}
			if want := filepath.Join(home, ".config", "rossoctl", "otel"); dir != want {
				t.Errorf("dir = %q, want the home-based fallback %q", dir, want)
			}
		})
	}
}

// TestConfigDirDefaultsUnderHome verifies the path with no XDG_CONFIG_HOME set.
func TestConfigDirDefaultsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	dir, ignored, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if ignored {
		t.Error("an unset XDG_CONFIG_HOME is not an ignored one")
	}
	if want := filepath.Join(home, ".config", "rossoctl", "otel"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}

// TestConfigDirIsUnderHome verifies the returned path is always inside the home
// directory, whatever XDG_CONFIG_HOME says.
//
// The property, rather than a specific path: it is the one thing the container
// mount depends on, so it is worth asserting directly.
func TestConfigDirIsUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	for _, xdg := range []string{"", filepath.Join(home, "c"), "/etc/xdg", "rel"} {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir, _, err := ConfigDir()
		if err != nil {
			t.Fatalf("ConfigDir with XDG_CONFIG_HOME=%q: %v", xdg, err)
		}
		rel, err := filepath.Rel(home, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("with XDG_CONFIG_HOME=%q, dir %q is not under home %q", xdg, dir, home)
		}
	}
}

// TestWriteConfigWritesMountableFile verifies the file is created, is valid YAML
// carrying the endpoint, and is named after the supplied time.
func TestWriteConfigWritesMountableFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "otel")
	const endpoint = "http://127.0.0.1:5999/v1/traces"
	now := time.Date(2026, 8, 17, 14, 30, 45, 0, time.UTC)

	path, err := WriteConfig(dir, NewConfig(endpoint), now)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	if want := filepath.Join(dir, "collector-20260817-143045.yaml"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the written config: %v", err)
	}
	var tree map[string]any
	if err := yaml.Unmarshal(data, &tree); err != nil {
		t.Fatalf("the written file is not valid YAML: %v\n%s", err, data)
	}
	if got := dig(t, tree, "exporters", "otlphttp/mlflow", "traces_endpoint"); got != endpoint {
		t.Errorf("written traces_endpoint = %v, want %q", got, endpoint)
	}
}

// TestWriteConfigTimestampsAvoidClobbering verifies two runs at different times
// write different files.
//
// This is what keeps a second `otel collect` from rewriting the file a running
// collector has bind-mounted: the container follows the host path, so overwriting
// it would change a live collector's configuration.
func TestWriteConfigTimestampsAvoidClobbering(t *testing.T) {
	dir := t.TempDir()
	first, err := WriteConfig(dir, NewConfig(DefaultTracesEndpoint), time.Date(2026, 8, 17, 14, 30, 45, 0, time.UTC))
	if err != nil {
		t.Fatalf("first WriteConfig: %v", err)
	}
	second, err := WriteConfig(dir, NewConfig(DefaultTracesEndpoint), time.Date(2026, 8, 17, 14, 30, 46, 0, time.UTC))
	if err != nil {
		t.Fatalf("second WriteConfig: %v", err)
	}
	if first == second {
		t.Errorf("both runs wrote %q; a second run must not overwrite a mounted config", first)
	}
}

// TestWriteConfigUsesUTC verifies the filename is in UTC.
//
// A local-time name would sort out of order across a DST change, and two runs an
// hour apart could produce the same name.
func TestWriteConfigUsesUTC(t *testing.T) {
	dir := t.TempDir()
	// 14:30 in a zone 5 hours behind UTC is 19:30 UTC.
	zone := time.FixedZone("TEST", -5*60*60)
	path, err := WriteConfig(dir, NewConfig(DefaultTracesEndpoint), time.Date(2026, 8, 17, 14, 30, 45, 0, zone))
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if got := filepath.Base(path); got != "collector-20260817-193045.yaml" {
		t.Errorf("filename = %q, want the UTC rendering collector-20260817-193045.yaml", got)
	}
}

// TestWriteRecord verifies the record file's contents and that it decodes to the
// two documented keys.
func TestWriteRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", RecordName)
	rec := Record{
		ConfigFile:   "/home/u/.config/rossoctl/otel/collector-20260817-143045.yaml",
		HTTPEndpoint: "0.0.0.0:4318",
	}
	if err := WriteRecord(path, rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	var got Record
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("the record is not valid YAML: %v\n%s", err, data)
	}
	if got != rec {
		t.Errorf("record = %+v, want %+v", got, rec)
	}
}

// TestListening verifies the probe against a real listener and a closed port.
func TestListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	if !Listening(port) {
		t.Errorf("Listening(%d) = false while a listener is open on it", port)
	}

	// Closing frees the port, so the same number now reports nothing there. Using
	// the just-closed port rather than an arbitrary one is what makes this
	// reliable: the kernel handed it out, so nothing else on the machine is
	// expected to grab it in between.
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if Listening(port) {
		t.Errorf("Listening(%d) = true after the listener closed", port)
	}
}

// TestPortsInUse verifies only the occupied ports are reported, in the order
// asked for.
func TestPortsInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	busy := ln.Addr().(*net.TCPAddr).Port

	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	if err := free.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := PortsInUse(freePort, busy)
	if len(got) != 1 || got[0] != busy {
		t.Errorf("PortsInUse(%d, %d) = %v, want only [%d]", freePort, busy, got, busy)
	}

	if got := PortsInUse(freePort); got != nil {
		t.Errorf("PortsInUse(%d) = %v, want nil when nothing is listening", freePort, got)
	}
}

// TestMLflowHint verifies the suggested command carries the flags that make MLflow
// reachable from inside the collector's container.
//
// Both flags are load-bearing and neither is MLflow's default: without
// --host 0.0.0.0 it binds loopback, which a container cannot reach, and without
// --allowed-hosts it rejects the collector's requests, whose Host header is
// host.containers.internal.
func TestMLflowHint(t *testing.T) {
	got := MLflowHint(5001)
	for _, want := range []string{"mlflow server", "--host 0.0.0.0", "--port 5001", "--allowed-hosts"} {
		if !strings.Contains(got, want) {
			t.Errorf("MLflowHint(5001) = %q, want it to contain %q", got, want)
		}
	}
}

// TestDefaultOTLPTracesURLMatchesHTTPPort verifies the constant URL and the port
// constant agree.
//
// DefaultOTLPTracesURL embeds the port as a literal so it can be a constant, which
// is exactly the arrangement that lets the two drift apart silently.
func TestDefaultOTLPTracesURLMatchesHTTPPort(t *testing.T) {
	if want := strconv.Itoa(HTTPPort); otlpHTTPPortString != want {
		t.Errorf("otlpHTTPPortString = %q, want %q to match HTTPPort", otlpHTTPPortString, want)
	}
	if want := "http://localhost:" + strconv.Itoa(HTTPPort) + "/v1/traces"; DefaultOTLPTracesURL != want {
		t.Errorf("DefaultOTLPTracesURL = %q, want %q", DefaultOTLPTracesURL, want)
	}
}

// TestNewMockTraceShape verifies the payload's structure and the fields a
// collector requires.
func TestNewMockTraceShape(t *testing.T) {
	end := time.Date(2026, 8, 17, 14, 30, 45, 123456789, time.UTC)
	payload, err := NewMockTrace("my-service", end)
	if err != nil {
		t.Fatalf("NewMockTrace: %v", err)
	}

	// Asserted through the JSON, not the struct: the wire field names are the
	// collector's interface, and a wrong tag would pass a struct comparison.
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var tree map[string]any
	if err := json.Unmarshal(data, &tree); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, data)
	}

	rs, ok := tree["resourceSpans"].([]any)
	if !ok || len(rs) != 1 {
		t.Fatalf("resourceSpans = %#v, want one entry", tree["resourceSpans"])
	}
	first := rs[0].(map[string]any)

	attrs := first["resource"].(map[string]any)["attributes"].([]any)
	if len(attrs) != 1 {
		t.Fatalf("attributes = %#v, want exactly one", attrs)
	}
	attr := attrs[0].(map[string]any)
	if attr["key"] != "service.name" {
		t.Errorf("attribute key = %v, want service.name", attr["key"])
	}
	if got := attr["value"].(map[string]any)["stringValue"]; got != "my-service" {
		t.Errorf("service.name stringValue = %v, want my-service", got)
	}

	spans := first["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)
	if len(spans) != 1 {
		t.Fatalf("spans = %#v, want exactly one", spans)
	}
	span := spans[0].(map[string]any)

	// The timestamps must be JSON strings. A nanosecond timestamp exceeds the
	// integers a JSON number is safely parsed into, so a number here could be
	// rounded through a float64 and shift the span in time.
	startRaw, ok := span["startTimeUnixNano"].(string)
	if !ok {
		t.Fatalf("startTimeUnixNano = %#v, want a string", span["startTimeUnixNano"])
	}
	endRaw, ok := span["endTimeUnixNano"].(string)
	if !ok {
		t.Fatalf("endTimeUnixNano = %#v, want a string", span["endTimeUnixNano"])
	}
	if want := strconv.FormatInt(end.UnixNano(), 10); endRaw != want {
		t.Errorf("endTimeUnixNano = %s, want %s (the supplied time)", endRaw, want)
	}
	if want := strconv.FormatInt(end.Add(-time.Second).UnixNano(), 10); startRaw != want {
		t.Errorf("startTimeUnixNano = %s, want %s (one second earlier)", startRaw, want)
	}

	if span["name"] != MockSpanName {
		t.Errorf("span name = %v, want %q", span["name"], MockSpanName)
	}
}

// TestNewMockTraceStartIsOneSecondBeforeEnd verifies the span's duration is exactly
// one second, whatever the clock reads.
func TestNewMockTraceStartIsOneSecondBeforeEnd(t *testing.T) {
	for _, end := range []time.Time{
		time.Date(2026, 8, 17, 14, 30, 45, 0, time.UTC),
		time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC), // just after an epoch-like boundary
		time.Now(),
	} {
		payload, err := NewMockTrace("svc", end)
		if err != nil {
			t.Fatalf("NewMockTrace: %v", err)
		}
		span := payload.ResourceSpans[0].ScopeSpans[0].Spans[0]
		start, err := strconv.ParseInt(span.StartTimeUnixNano, 10, 64)
		if err != nil {
			t.Fatalf("start is not an integer: %v", err)
		}
		finish, err := strconv.ParseInt(span.EndTimeUnixNano, 10, 64)
		if err != nil {
			t.Fatalf("end is not an integer: %v", err)
		}
		if finish-start != int64(time.Second) {
			t.Errorf("duration = %dns, want exactly 1s", finish-start)
		}
	}
}

// TestNewMockTraceIDsAreRandomAndWellFormed verifies the IDs are the right length,
// are lowercase hex, are not all zeros, and differ between calls.
//
// The length and charset are what OTLP requires; the all-zero exclusion matters
// because OTLP defines such an ID as invalid, and a collector may drop the span.
func TestNewMockTraceIDsAreRandomAndWellFormed(t *testing.T) {
	const runs = 25
	traceIDs := make(map[string]bool, runs)
	spanIDs := make(map[string]bool, runs)

	for range runs {
		payload, err := NewMockTrace("svc", time.Now())
		if err != nil {
			t.Fatalf("NewMockTrace: %v", err)
		}
		traceID, spanID := payload.TraceID(), payload.SpanID()

		for _, tc := range []struct {
			name   string
			id     string
			hexLen int
		}{
			{"traceId", traceID, TraceIDLen * 2},
			{"spanId", spanID, SpanIDLen * 2},
		} {
			if len(tc.id) != tc.hexLen {
				t.Fatalf("%s = %q, want %d hex characters", tc.name, tc.id, tc.hexLen)
			}
			raw, err := hex.DecodeString(tc.id)
			if err != nil {
				t.Fatalf("%s = %q is not hex: %v", tc.name, tc.id, err)
			}
			if strings.ToLower(tc.id) != tc.id {
				t.Errorf("%s = %q, want lowercase hex", tc.name, tc.id)
			}
			if allZero(raw) {
				t.Errorf("%s = %q is all zeros, which OTLP treats as invalid", tc.name, tc.id)
			}
		}
		traceIDs[traceID] = true
		spanIDs[spanID] = true
	}

	// Every ID distinct across the runs. A fixed or seeded-once generator would
	// collapse these to one entry.
	if len(traceIDs) != runs {
		t.Errorf("got %d distinct trace IDs across %d runs; they must be random", len(traceIDs), runs)
	}
	if len(spanIDs) != runs {
		t.Errorf("got %d distinct span IDs across %d runs; they must be random", len(spanIDs), runs)
	}
}

// TestSendTraceposts verifies the request's method, path, content type, and body.
func TestSendTracePosts(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	payload, err := NewMockTrace("svc", time.Now())
	if err != nil {
		t.Fatalf("NewMockTrace: %v", err)
	}
	partial, err := SendTrace(context.Background(), srv.Client(), srv.URL+"/v1/traces", payload)
	if err != nil {
		t.Fatalf("SendTrace: %v", err)
	}
	if partial != nil {
		t.Errorf("partial = %+v, want nil for a clean success", partial)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/traces" {
		t.Errorf("path = %q, want /v1/traces", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}
	if _, ok := gotBody["resourceSpans"]; !ok {
		t.Errorf("body has no resourceSpans: %+v", gotBody)
	}
}

// TestSendTracePartialSuccess verifies a 200 that reports rejected spans is
// surfaced rather than read as success.
//
// This is the case a status-code-only check gets wrong: the request succeeded and
// the span did not land.
func TestSendTracePartialSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// rejectedSpans is a string in OTLP/JSON, being a 64-bit integer.
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":"1","errorMessage":"bad span"}}`))
	}))
	defer srv.Close()

	payload, err := NewMockTrace("svc", time.Now())
	if err != nil {
		t.Fatalf("NewMockTrace: %v", err)
	}
	partial, err := SendTrace(context.Background(), srv.Client(), srv.URL, payload)
	if err != nil {
		t.Fatalf("SendTrace: %v", err)
	}
	if partial == nil {
		t.Fatal("a partial success must be reported, not treated as a clean send")
	}
	if partial.RejectedSpans != 1 || partial.ErrorMessage != "bad span" {
		t.Errorf("partial = %+v, want 1 rejected span with the message", partial)
	}
}

// TestSendTraceEmptyAndNonJSONBodies verifies a success with nothing useful in the
// body is treated as a clean send.
//
// Both are real: the OTLP spec allows an empty body on success, and something that
// is not a collector may answer 200 with prose. Neither is worth failing over once
// the status says the span was accepted.
func TestSendTraceEmptyAndNonJSONBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"whitespace", "  \n"},
		{"not json", "OK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			payload, err := NewMockTrace("svc", time.Now())
			if err != nil {
				t.Fatalf("NewMockTrace: %v", err)
			}
			partial, err := SendTrace(context.Background(), srv.Client(), srv.URL, payload)
			if err != nil {
				t.Errorf("SendTrace: %v", err)
			}
			if partial != nil {
				t.Errorf("partial = %+v, want nil", partial)
			}
		})
	}
}

// TestSendTraceHTTPError verifies a non-2xx status fails and carries the
// collector's own explanation, which is the part that says what was wrong.
func TestSendTraceHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid trace id"))
	}))
	defer srv.Close()

	payload, err := NewMockTrace("svc", time.Now())
	if err != nil {
		t.Fatalf("NewMockTrace: %v", err)
	}
	_, err = SendTrace(context.Background(), srv.Client(), srv.URL, payload)
	if err == nil {
		t.Fatal("expected an error for HTTP 400")
	}
	for _, want := range []string{"400", "invalid trace id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// TestSendTraceUnreachable verifies a refused connection names the URL that was
// tried, since the usual cause is that no collector is running.
func TestSendTraceUnreachable(t *testing.T) {
	// A port that was just released, so the dial is refused rather than hanging.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "http://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + "/v1/traces"
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	payload, err := NewMockTrace("svc", time.Now())
	if err != nil {
		t.Fatalf("NewMockTrace: %v", err)
	}
	_, err = SendTrace(context.Background(), nil, url, payload)
	if err == nil {
		t.Fatal("expected an error when nothing is listening")
	}
	if !strings.Contains(err.Error(), url) {
		t.Errorf("error = %v, want it to name %q", err, url)
	}
}
