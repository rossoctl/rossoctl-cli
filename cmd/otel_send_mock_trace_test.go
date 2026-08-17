package cmd

import (
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// captureTraceServer serves an OTLP traces endpoint, decoding each request body
// into gotBody, and returns its /v1/traces URL.
func captureTraceServer(t *testing.T, gotBody *map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(gotBody); err != nil {
			t.Errorf("decoding the trace body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/traces"
}

// span digs out the single span of a captured OTLP payload, failing the test if
// the shape is not the expected one-resource, one-scope, one-span tree.
func span(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	rs, ok := body["resourceSpans"].([]any)
	if !ok || len(rs) != 1 {
		t.Fatalf("resourceSpans = %#v, want one entry", body["resourceSpans"])
	}
	scope, ok := rs[0].(map[string]any)["scopeSpans"].([]any)
	if !ok || len(scope) != 1 {
		t.Fatalf("scopeSpans = %#v, want one entry", rs[0])
	}
	spans, ok := scope[0].(map[string]any)["spans"].([]any)
	if !ok || len(spans) != 1 {
		t.Fatalf("spans = %#v, want one entry", scope[0])
	}
	s, ok := spans[0].(map[string]any)
	if !ok {
		t.Fatalf("span = %#v, want an object", spans[0])
	}
	return s
}

// serviceName digs out the payload's service.name resource attribute.
func serviceName(t *testing.T, body map[string]any) string {
	t.Helper()
	rs := body["resourceSpans"].([]any)
	resource, ok := rs[0].(map[string]any)["resource"].(map[string]any)
	if !ok {
		t.Fatalf("resource = %#v, want an object", rs[0])
	}
	attrs, ok := resource["attributes"].([]any)
	if !ok {
		t.Fatalf("attributes = %#v, want an array", resource["attributes"])
	}
	for _, raw := range attrs {
		attr := raw.(map[string]any)
		if attr["key"] != "service.name" {
			continue
		}
		value, ok := attr["value"].(map[string]any)
		if !ok {
			t.Fatalf("service.name value = %#v, want an object", attr["value"])
		}
		s, ok := value["stringValue"].(string)
		if !ok {
			t.Fatalf("service.name has no stringValue: %#v", value)
		}
		return s
	}
	t.Fatalf("no service.name attribute in %#v", attrs)
	return ""
}

// TestOtelSendMockTraceDefaultServiceName verifies the default service name and the
// overall payload shape reaching the collector.
func TestOtelSendMockTraceDefaultServiceName(t *testing.T) {
	var body map[string]any
	url := captureTraceServer(t, &body)

	out, err := execute(t, "otel", "send-mock-trace", "--url", url)
	if err != nil {
		t.Fatalf("send-mock-trace: %v", err)
	}

	if got := serviceName(t, body); got != "rossoctl-cli" {
		t.Errorf("service.name = %q, want the default rossoctl-cli", got)
	}
	// The IDs are reported so a user can find the trace in a viewer.
	s := span(t, body)
	if !strings.Contains(out, s["traceId"].(string)) {
		t.Errorf("output does not name the trace ID that was sent:\n%s", out)
	}
}

// TestOtelSendMockTraceServiceNameFlag verifies --serviceName sets the resource
// attribute's stringValue.
func TestOtelSendMockTraceServiceNameFlag(t *testing.T) {
	var body map[string]any
	url := captureTraceServer(t, &body)

	if _, err := execute(t, "otel", "send-mock-trace", "--url", url, "--serviceName", "my-agent"); err != nil {
		t.Fatalf("send-mock-trace: %v", err)
	}
	if got := serviceName(t, body); got != "my-agent" {
		t.Errorf("service.name = %q, want my-agent", got)
	}
}

// TestOtelSendMockTraceDefaultFlags verifies the documented defaults.
func TestOtelSendMockTraceDefaultFlags(t *testing.T) {
	for _, tc := range []struct{ flag, want string }{
		{"serviceName", "rossoctl-cli"},
		{"url", "http://localhost:4318/v1/traces"},
	} {
		f := otelSendMockTraceCmd.Flags().Lookup(tc.flag)
		if f == nil {
			t.Errorf("send-mock-trace has no --%s flag", tc.flag)
			continue
		}
		if f.DefValue != tc.want {
			t.Errorf("--%s default = %q, want %q", tc.flag, f.DefValue, tc.want)
		}
	}
}

// TestOtelSendMockTraceIDsAreRandom verifies two runs send different trace and span
// IDs, which is what makes each invocation a distinct trace.
func TestOtelSendMockTraceIDsAreRandom(t *testing.T) {
	var body map[string]any
	url := captureTraceServer(t, &body)

	if _, err := execute(t, "otel", "send-mock-trace", "--url", url); err != nil {
		t.Fatalf("first send: %v", err)
	}
	first := span(t, body)
	firstTrace, firstSpan := first["traceId"].(string), first["spanId"].(string)

	body = nil
	if _, err := execute(t, "otel", "send-mock-trace", "--url", url); err != nil {
		t.Fatalf("second send: %v", err)
	}
	second := span(t, body)

	if second["traceId"] == firstTrace {
		t.Errorf("both runs sent trace ID %s; each run must be a new trace", firstTrace)
	}
	if second["spanId"] == firstSpan {
		t.Errorf("both runs sent span ID %s", firstSpan)
	}
}

// TestOtelSendMockTraceIDsAreWellFormed verifies the wire IDs are hex of the
// lengths OTLP requires: 32 characters for a trace, 16 for a span.
func TestOtelSendMockTraceIDsAreWellFormed(t *testing.T) {
	var body map[string]any
	url := captureTraceServer(t, &body)

	if _, err := execute(t, "otel", "send-mock-trace", "--url", url); err != nil {
		t.Fatalf("send-mock-trace: %v", err)
	}

	s := span(t, body)
	for _, tc := range []struct {
		field  string
		hexLen int
	}{
		{"traceId", 32},
		{"spanId", 16},
	} {
		got, ok := s[tc.field].(string)
		if !ok {
			t.Fatalf("%s = %#v, want a string", tc.field, s[tc.field])
		}
		if len(got) != tc.hexLen {
			t.Errorf("%s = %q, want %d hex characters", tc.field, got, tc.hexLen)
		}
		if _, err := hex.DecodeString(got); err != nil {
			t.Errorf("%s = %q is not hex: %v", tc.field, got, err)
		}
	}
}

// TestOtelSendMockTraceTimestamps verifies the span ends at the current time and
// starts one second earlier, and that both are sent as strings.
//
// The string form is required: a nanosecond timestamp exceeds the integers a JSON
// number is safely parsed into, so a number could be rounded through a float64 and
// move the span in time.
func TestOtelSendMockTraceTimestamps(t *testing.T) {
	var body map[string]any
	url := captureTraceServer(t, &body)

	at := time.Date(2026, 8, 17, 14, 30, 45, 123456789, time.UTC)
	prev := timeNowOtel
	timeNowOtel = func() time.Time { return at }
	t.Cleanup(func() { timeNowOtel = prev })

	if _, err := execute(t, "otel", "send-mock-trace", "--url", url); err != nil {
		t.Fatalf("send-mock-trace: %v", err)
	}

	s := span(t, body)
	start, ok := s["startTimeUnixNano"].(string)
	if !ok {
		t.Fatalf("startTimeUnixNano = %#v, want a string", s["startTimeUnixNano"])
	}
	end, ok := s["endTimeUnixNano"].(string)
	if !ok {
		t.Fatalf("endTimeUnixNano = %#v, want a string", s["endTimeUnixNano"])
	}

	if want := strconv.FormatInt(at.UnixNano(), 10); end != want {
		t.Errorf("endTimeUnixNano = %s, want the current time %s", end, want)
	}
	if want := strconv.FormatInt(at.Add(-time.Second).UnixNano(), 10); start != want {
		t.Errorf("startTimeUnixNano = %s, want one second earlier: %s", start, want)
	}
}

// TestOtelSendMockTraceUsesCurrentTime verifies the timestamps track the real clock
// when nothing is stubbed, rather than being a fixed or zero value.
func TestOtelSendMockTraceUsesCurrentTime(t *testing.T) {
	var body map[string]any
	url := captureTraceServer(t, &body)

	before := time.Now().Add(-time.Second)
	if _, err := execute(t, "otel", "send-mock-trace", "--url", url); err != nil {
		t.Fatalf("send-mock-trace: %v", err)
	}
	after := time.Now().Add(time.Second)

	end, err := strconv.ParseInt(span(t, body)["endTimeUnixNano"].(string), 10, 64)
	if err != nil {
		t.Fatalf("endTimeUnixNano is not an integer: %v", err)
	}
	if end < before.UnixNano() || end > after.UnixNano() {
		t.Errorf("endTimeUnixNano %d is outside the window this test ran in", end)
	}
}

// TestOtelSendMockTraceRejectsEmptyServiceName verifies an empty --serviceName fails
// and sends nothing.
//
// A collector accepts an empty service.name and then groups the span under nothing,
// which is a confusing way to learn the flag got an empty value.
func TestOtelSendMockTraceRejectsEmptyServiceName(t *testing.T) {
	var body map[string]any
	url := captureTraceServer(t, &body)

	_, err := execute(t, "otel", "send-mock-trace", "--url", url, "--serviceName", "")
	if err == nil {
		t.Fatal("expected an error for an empty --serviceName")
	}
	if !strings.Contains(err.Error(), "serviceName") {
		t.Errorf("error should name the flag: %v", err)
	}
	if body != nil {
		t.Errorf("nothing should have been sent, but the server received %+v", body)
	}
}

// TestOtelSendMockTraceUnreachable verifies a refused connection fails with the URL
// and a pointer to the command that starts a collector.
func TestOtelSendMockTraceUnreachable(t *testing.T) {
	// A port that was just released, so the dial is refused rather than hanging.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "http://127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + "/v1/traces"
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = execute(t, "otel", "send-mock-trace", "--url", url)
	if err == nil {
		t.Fatal("expected an error when nothing is listening")
	}
	if !strings.Contains(err.Error(), url) {
		t.Errorf("error should name the URL tried: %v", err)
	}
	if !strings.Contains(err.Error(), "otel collect") {
		t.Errorf("error should suggest starting a collector: %v", err)
	}
}

// TestOtelSendMockTraceReportsPartialSuccess verifies a 200 that reports rejected
// spans fails the command.
//
// Otherwise a script checking the exit status would be told the trace landed when
// the collector said it had dropped it.
func TestOtelSendMockTraceReportsPartialSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":"1","errorMessage":"nope"}}`))
	}))
	t.Cleanup(srv.Close)

	_, err := execute(t, "otel", "send-mock-trace", "--url", srv.URL+"/v1/traces")
	if err == nil {
		t.Fatal("expected an error when the collector reports a rejected span")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should carry the collector's message: %v", err)
	}
}

// TestOtelSendMockTraceHTTPError verifies a non-2xx response fails the command.
func TestOtelSendMockTraceHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("collector busy"))
	}))
	t.Cleanup(srv.Close)

	_, err := execute(t, "otel", "send-mock-trace", "--url", srv.URL+"/v1/traces")
	if err == nil {
		t.Fatal("expected an error for HTTP 503")
	}
	for _, want := range []string{"503", "collector busy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// TestOtelSendMockTraceRejectsArgs verifies the command takes no positional
// arguments, so a mistyped flag is reported rather than ignored.
func TestOtelSendMockTraceRejectsArgs(t *testing.T) {
	if _, err := execute(t, "otel", "send-mock-trace", "unexpected"); err == nil {
		t.Error("expected an error for a positional argument")
	}
}

// TestOtelSendMockTraceInGroupHelp verifies the subcommand is listed under `otel`.
func TestOtelSendMockTraceInGroupHelp(t *testing.T) {
	out, err := execute(t, "otel")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "send-mock-trace") {
		t.Errorf("`otel` help missing send-mock-trace:\n%s", out)
	}
}
