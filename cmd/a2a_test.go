package cmd

import (
	"bytes"
	"context"
	"iter"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// echoExecutor is a minimal A2A agent that streams back a status update and a
// message echoing what it was sent. It gives the send tests a real server to
// talk to, so the streaming loop and event printing are exercised over an
// actual JSON-RPC transport rather than against a stub client.
type echoExecutor struct {
	// authHeader records the Authorization header of the last request, so the
	// --with-authorization test can assert the token actually reached the wire.
	authHeader string
}

func (e *echoExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// The server requires the first event to be a Task or a Message, so the
		// task is emitted before any status update on it.
		task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
		if !yield(task, nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil), nil) {
			return
		}
		// Once a task is stored the server rejects a bare Message event, so the
		// echoed text is carried on the terminal status update instead.
		text := messageText(execCtx.Message)
		reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("echo: "+text))
		yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, reply), nil)
	}
}

func (e *echoExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// newEchoServer starts an httptest server speaking the A2A JSON-RPC binding,
// backed by echoExecutor.
func newEchoServer(t *testing.T) (*httptest.Server, *echoExecutor) {
	t.Helper()

	exec := &echoExecutor{}
	jsonrpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec))

	// Capture the Authorization header before delegating, so tests can assert
	// on what the client sent.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exec.authHeader = r.Header.Get("Authorization")
		jsonrpc.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, exec
}

// artifactExecutor is an agent that emits an artifact carrying a structured
// error, mirroring how an agent reports a failure as artifact content rather
// than in the task status.
type artifactExecutor struct{}

func (artifactExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
		if !yield(task, nil) {
			return
		}
		if !yield(&a2a.TaskArtifactUpdateEvent{
			TaskID:    task.ID,
			ContextID: task.ContextID,
			Artifact: &a2a.Artifact{
				ID:   "art-err",
				Name: "crash-report",
				Parts: a2a.ContentParts{
					a2a.NewTextPart("stack overflow"),
					a2a.NewDataPart(map[string]any{"code": "E42"}),
				},
			},
		}, nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateCompleted, nil), nil)
	}
}

func (artifactExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// newArtifactServer starts an A2A JSON-RPC server backed by artifactExecutor.
func newArtifactServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(artifactExecutor{})))
	t.Cleanup(srv.Close)
	return srv
}

func TestA2ASendStreamsEvents(t *testing.T) {
	srv, _ := newEchoServer(t)

	out, err := execute(t, "a2a", "send", "--address", srv.URL, "--message", "hello there")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}

	// Both streamed events should be printed, in arrival order.
	if !strings.Contains(out, "echo: hello there") {
		t.Errorf("output missing echoed message:\n%s", out)
	}
	if !strings.Contains(out, "working") && !strings.Contains(out, "WORKING") {
		t.Errorf("output missing the working status update:\n%s", out)
	}
}

func TestA2ASendDefaultsToJSONRPC(t *testing.T) {
	srv, _ := newEchoServer(t)

	// No --transport: the jsonrpc default must be what reaches the server,
	// which only speaks the JSON-RPC binding.
	out, err := execute(t, "a2a", "send", "--address", srv.URL, "--message", "hi")
	if err != nil {
		t.Fatalf("unexpected error with default transport: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "echo: hi") {
		t.Errorf("default transport did not reach the agent:\n%s", out)
	}
}

func TestA2ASendRequiredFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no address", []string{"a2a", "send", "--message", "hi"}, "--address is required"},
		{"no message", []string{"a2a", "send", "--address", "http://example.invalid"}, "--message is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execute(t, tc.args...)
			if err == nil {
				t.Fatalf("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestA2ASendRejectsUnknownTransport(t *testing.T) {
	_, err := execute(t, "a2a", "send",
		"--address", "http://example.invalid", "--message", "hi", "--transport", "carrier-pigeon")
	if err == nil {
		t.Fatal("expected an error for an unknown transport, got none")
	}
	if !strings.Contains(err.Error(), "unknown --transport") {
		t.Errorf("error = %q, want it to mention an unknown transport", err)
	}
}

func TestA2ATransportAliases(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want a2a.TransportProtocol
	}{
		{"jsonrpc", a2a.TransportProtocolJSONRPC},
		{"JSONRPC", a2a.TransportProtocolJSONRPC},
		{"json-rpc", a2a.TransportProtocolJSONRPC},
		{" grpc ", a2a.TransportProtocolGRPC},
		{"http+json", a2a.TransportProtocolHTTPJSON},
		{"rest", a2a.TransportProtocolHTTPJSON},
	} {
		got, err := a2aTransport(tc.in)
		if err != nil {
			t.Errorf("a2aTransport(%q) returned error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("a2aTransport(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPrintA2AEventArtifactParts covers the artifact branch of printA2AEvent.
// An artifact's payload is its parts, and an agent may report a failure there, so
// the parts must be printed and not summarized away by name alone.
func TestPrintA2AEventArtifactParts(t *testing.T) {
	tests := []struct {
		name     string
		artifact *a2a.Artifact
		want     []string
		notWant  []string
	}{
		{
			name: "text part is printed",
			artifact: &a2a.Artifact{
				ID:    "art-1",
				Name:  "report",
				Parts: a2a.ContentParts{a2a.NewTextPart("traceback: boom")},
			},
			want: []string{"report", "traceback: boom"},
		},
		{
			name: "structured error data is printed as JSON",
			artifact: &a2a.Artifact{
				ID:   "art-2",
				Name: "failure",
				Parts: a2a.ContentParts{a2a.NewDataPart(map[string]any{
					"error": "upstream unreachable",
				})},
			},
			want: []string{"failure", `"error"`, "upstream unreachable"},
		},
		{
			name: "name falls back to the artifact ID",
			artifact: &a2a.Artifact{
				ID:    "art-3",
				Parts: a2a.ContentParts{a2a.NewTextPart("body")},
			},
			want: []string{"art-3", "body"},
		},
		{
			name:     "an artifact with no parts still prints its name",
			artifact: &a2a.Artifact{ID: "art-4", Name: "empty"},
			want:     []string{"empty"},
			// No trailing separator when there is no content to append.
			notWant: []string{"empty: "},
		},
		{
			name: "a nil part is skipped rather than panicking",
			artifact: &a2a.Artifact{
				ID:    "art-5",
				Name:  "mixed",
				Parts: a2a.ContentParts{nil, a2a.NewTextPart("survived")},
			},
			want: []string{"mixed", "survived"},
		},
		{
			// Artifact is a pointer field, so a malformed peer response can leave
			// it nil. That must be reported, not panic.
			name:     "a nil artifact is reported",
			artifact: nil,
			want:     []string{"task-9", "<missing>"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printA2AEvent(&buf, &a2a.TaskArtifactUpdateEvent{
				TaskID:   "task-9",
				Artifact: tc.artifact,
			})
			got := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("output %q missing %q", got, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("output %q should not contain %q", got, notWant)
				}
			}
		})
	}
}

// TestPartTextData covers the data-part rendering that previously returned "",
// which made a structured error print as nothing at all.
func TestPartTextData(t *testing.T) {
	tests := []struct {
		name string
		part *a2a.Part
		want string
	}{
		{"map is compact JSON", a2a.NewDataPart(map[string]any{"code": "E42"}), `{"code":"E42"}`},
		{"slice is compact JSON", a2a.NewDataPart([]any{1.0, "two"}), `[1,"two"]`},
		{"string value is quoted JSON", a2a.NewDataPart("plain"), `"plain"`},
		{"nil value is labelled", a2a.NewDataPart(nil), "[data]"},
		// A value JSON cannot represent must be reported, not silently dropped.
		{"unmarshalable value is reported", a2a.NewDataPart(math.NaN()), "[unprintable data:"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := partText(tc.part); !strings.Contains(got, tc.want) {
				t.Errorf("partText() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestPartTextKinds pins the rendering of the non-data part kinds, so the
// dataText addition is not mistaken for a change to the others.
func TestPartTextKinds(t *testing.T) {
	if got, want := partText(a2a.NewTextPart("hello")), "hello"; got != want {
		t.Errorf("text part = %q, want %q", got, want)
	}
	if got := partText(a2a.NewFileURLPart("https://example.com/f.pdf", "application/pdf")); !strings.Contains(got, "https://example.com/f.pdf") {
		t.Errorf("url part = %q, want it to name the URL", got)
	}
	raw := a2a.NewRawPart([]byte("abcd"))
	raw.Filename = "blob.bin"
	if got := partText(raw); !strings.Contains(got, "blob.bin") || !strings.Contains(got, "4 bytes") {
		t.Errorf("raw part = %q, want the filename and byte count", got)
	}
	// An unset Content must render to nothing rather than panicking.
	if got := partText(&a2a.Part{}); got != "" {
		t.Errorf("empty part = %q, want %q", got, "")
	}
}

// TestA2ASendPrintsArtifactEndToEnd drives a real agent that emits an artifact
// carrying a structured error, so the artifact path is covered over an actual
// stream and not just by calling the printer directly.
func TestA2ASendPrintsArtifactEndToEnd(t *testing.T) {
	srv := newArtifactServer(t)

	out, err := execute(t, "a2a", "send", "--address", srv.URL, "--message", "go")
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	for _, want := range []string{"crash-report", "stack overflow", `"code"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestA2ASendWithAuthorizationSendsHeader checks that --with-authorization puts
// the context's token on the wire as a bearer credential, and that omitting the
// flag sends no Authorization header at all.
func TestA2ASendWithAuthorizationSendsHeader(t *testing.T) {
	srv, exec := newEchoServer(t)

	// Point the context at a config carrying a token. resolveServer reads the
	// current context, so the token has to come from a real config file; the
	// isolated HOME keeps it out of the developer's real config.
	isolateHome(t)
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	if _, err := execute(t, "login", "--token", "s3cr3t"); err != nil {
		t.Fatalf("login: %v", err)
	}

	if _, err := execute(t, "a2a", "send",
		"--address", srv.URL, "--message", "hi", "--with-authorization"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := exec.authHeader, "Bearer s3cr3t"; got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}

	// Without the flag, nothing should be attached.
	exec.authHeader = ""
	if _, err := execute(t, "a2a", "send", "--address", srv.URL, "--message", "hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exec.authHeader != "" {
		t.Errorf("Authorization header = %q, want it unset without --with-authorization", exec.authHeader)
	}
}

// TestA2ASendWithAuthorizationNoToken checks that --with-authorization fails
// with a clear message when the context has no token, rather than silently
// sending an unauthenticated request. The test HOME is empty, so the seeded
// default context has no bearer token.
func TestA2ASendWithAuthorizationNoToken(t *testing.T) {
	_, err := execute(t, "a2a", "send",
		"--address", "http://example.invalid", "--message", "hi", "--with-authorization")
	if err == nil {
		t.Fatal("expected an error when no token is available, got none")
	}
	if !strings.Contains(err.Error(), "no bearer token") {
		t.Errorf("error = %q, want it to mention the missing bearer token", err)
	}
}

// newRejectingAgent starts a server that answers every request with the given
// status, standing in for an agent behind a proxy that rejects the call.
func newRejectingAgent(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestA2AVerboseLogsRequests verifies --verbose reports the A2A calls on stderr
// while stdout keeps only the streamed events. Before the logging transport the
// A2A calls were invisible under -v: it instrumented the platform API client
// only, so the request the command exists to make was the one thing not logged.
func TestA2AVerboseLogsRequests(t *testing.T) {
	srv, _ := newEchoServer(t)

	stdout, stderr, err := executeSplit(t, "a2a", "send",
		"--address", srv.URL, "--message", "hi", "--verbose")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The request and its status, both naming the agent's URL.
	if !strings.Contains(stderr, "POST "+srv.URL) {
		t.Errorf("stderr missing the request line:\n%s", stderr)
	}
	if !strings.Contains(stderr, "200 OK") {
		t.Errorf("stderr missing the response status:\n%s", stderr)
	}
	// Verbose output must not contaminate the results.
	if strings.Contains(stdout, "POST ") {
		t.Errorf("verbose logging leaked into stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "echo: hi") {
		t.Errorf("stdout missing the streamed events:\n%s", stdout)
	}
}

// TestA2AQuietWithoutVerbose verifies the logging transport stays silent when
// --verbose is not given, since it is installed either way.
func TestA2AQuietWithoutVerbose(t *testing.T) {
	srv, _ := newEchoServer(t)

	_, stderr, err := executeSplit(t, "a2a", "send", "--address", srv.URL, "--message", "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stderr != "" {
		t.Errorf("stderr should be empty without --verbose, got:\n%s", stderr)
	}
}

// TestA2AVerboseLogsFailedRequest verifies a request that never got a response
// is still reported, rather than -v going quiet exactly when it is most useful.
func TestA2AVerboseLogsFailedRequest(t *testing.T) {
	// A server closed immediately: the port is refused rather than unroutable, so
	// this fails fast instead of waiting for a dial timeout.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := dead.URL
	dead.Close()

	_, stderr, err := executeSplit(t, "a2a", "send",
		"--address", addr, "--message", "hi", "--verbose")
	if err == nil {
		t.Fatal("sending to a closed port should fail")
	}
	if !strings.Contains(stderr, "failed after") {
		t.Errorf("stderr should report the failed request:\n%s", stderr)
	}
}

// TestA2AVerboseLogsHTTPJSONTransport verifies the HTTP+JSON binding is logged
// too. Each binding gets its own transport built on the logging client, so this
// pins that the second registration was not forgotten.
func TestA2AVerboseLogsHTTPJSONTransport(t *testing.T) {
	srv := newRejectingAgent(t, http.StatusOK)

	_, stderr, _ := executeSplit(t, "a2a", "send",
		"--address", srv.URL, "--message", "hi",
		"--transport", "http+json", "--verbose")
	if !strings.Contains(stderr, srv.URL) {
		t.Errorf("stderr missing the http+json request:\n%s", stderr)
	}
}

// TestA2AUnauthorizedHint covers the advice offered for a rejected call
// directly, including the cases that must stay silent.
func TestA2AUnauthorizedHint(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		withAuth  bool
		wantEmpty bool
		wants     []string
	}{
		{
			name:   "401 without the flag names the flag",
			status: http.StatusUnauthorized,
			wants:  []string{"--with-authorization"},
		},
		{
			name:     "401 with the flag names auth status and login",
			status:   http.StatusUnauthorized,
			withAuth: true,
			wants:    []string{"auth status", "login"},
		},
		// A 403 is an authenticated identity without permission: neither adding a
		// token nor inspecting one changes the answer.
		{name: "403 is permission, not credentials", status: http.StatusForbidden, wantEmpty: true},
		{name: "500", status: http.StatusInternalServerError, wantEmpty: true},
		{name: "200", status: http.StatusOK, wantEmpty: true},
		// No response ever arrived, so there is no status to reason about.
		{name: "no response", status: 0, wantEmpty: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := a2aUnauthorizedHint(tc.status, tc.withAuth)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("hint = %q, want none", got)
				}
				return
			}
			if got == "" {
				t.Fatal("expected a hint")
			}
			for _, want := range tc.wants {
				if !strings.Contains(got, want) {
					t.Errorf("hint = %q, want it to mention %q", got, want)
				}
			}
		})
	}
}

// TestA2ASend401HintEndToEnd verifies the hint reaches stderr from a real
// rejected call, and that the command still fails: the hint is advice, not a
// recovery.
func TestA2ASend401HintEndToEnd(t *testing.T) {
	srv := newRejectingAgent(t, http.StatusUnauthorized)

	stdout, stderr, err := executeSplit(t, "a2a", "send", "--address", srv.URL, "--message", "hi")
	if err == nil {
		t.Fatal("a 401 should fail the command")
	}
	if !strings.Contains(stderr, "--with-authorization") {
		t.Errorf("stderr missing the hint:\n%s", stderr)
	}
	// Advice belongs on stderr, so piped output stays usable.
	if strings.Contains(stdout, "Hint:") {
		t.Errorf("hint leaked into stdout:\n%s", stdout)
	}
}
