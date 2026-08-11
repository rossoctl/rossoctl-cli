package inprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/instances"
	"github.com/rossoctl/rossoctl-cli/internal/serve"
)

// handlerFunc adapts a function to http.Handler, so each test can state the
// server side it needs inline.
type handlerFunc func(http.ResponseWriter, *http.Request)

func (f handlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) { f(w, r) }

// trackedBody is a request body that records whether it was closed, which is the
// RoundTripper obligation most likely to regress: nothing in normal use fails
// visibly when a body is leaked.
type trackedBody struct {
	io.Reader
	mu     sync.Mutex
	closed bool
}

func newTrackedBody(s string) *trackedBody {
	return &trackedBody{Reader: strings.NewReader(s)}
}

func (b *trackedBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (b *trackedBody) wasClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func TestRoundTripServesHandler(t *testing.T) {
	tr := &Transport{Handler: handlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Custom", "yes")
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, `{"ok":true}`)
	})}

	req, err := http.NewRequest(http.MethodGet, "http://cortex/api/v1/agents", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
	if got := resp.Header.Get("X-Custom"); got != "yes" {
		t.Errorf("X-Custom = %q, want %q", got, "yes")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want %q", body, `{"ok":true}`)
	}
	if resp.Request != req {
		t.Error("Request is not the request that was passed in")
	}
}

// TestRoundTripDefaultsStatusToOK pins the reason httptest.NewRecorder is used
// rather than a hand-rolled ResponseWriter: a handler that writes a body without
// calling WriteHeader must still produce a 200, as it would over a socket.
func TestRoundTripDefaultsStatusToOK(t *testing.T) {
	tr := &Transport{Handler: handlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "hello")
	})}

	req, _ := http.NewRequest(http.MethodGet, "http://cortex/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestRoundTripAlwaysClosesBody covers all three exits, including the two error
// returns. The contract requires the body be closed even when the transport
// refuses to serve the request.
func TestRoundTripAlwaysClosesBody(t *testing.T) {
	okHandler := handlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, tc := range []struct {
		name    string
		tr      *Transport
		nilURL  bool
		wantErr bool
	}{
		{name: "success", tr: &Transport{Handler: okHandler}},
		{name: "nil handler", tr: &Transport{}, wantErr: true},
		{name: "nil URL", tr: &Transport{Handler: okHandler}, nilURL: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := newTrackedBody("payload")
			req, err := http.NewRequest(http.MethodPost, "http://cortex/api/v1/agents", body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if tc.nilURL {
				req.URL = nil
			}

			resp, err := tc.tr.RoundTrip(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("RoundTrip returned %#v, want an error", resp)
				}
				if resp != nil {
					t.Errorf("response = %#v, want nil alongside the error", resp)
				}
			} else {
				if err != nil {
					t.Fatalf("RoundTrip: %v", err)
				}
				resp.Body.Close()
			}

			if !body.wasClosed() {
				t.Error("request body was not closed")
			}
		})
	}
}

// TestRoundTripDoesNotModifyRequest pins the "must not modify the request" half
// of the contract, against a handler that does the two things ServeMux and
// serve's own handlers do: read path values and set headers.
func TestRoundTripDoesNotModifyRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		// Reading these is why the handler gets a clone: ServeMux stores path
		// values on the request it dispatches.
		if r.PathValue("namespace") != "team1" || r.PathValue("name") != "bot" {
			t.Errorf("path values = %q/%q, want team1/bot",
				r.PathValue("namespace"), r.PathValue("name"))
		}
		if r.RequestURI == "" {
			t.Error("RequestURI is empty; a handler should see what a socket would have supplied")
		}
		if r.RemoteAddr == "" {
			t.Error("RemoteAddr is empty")
		}
		if r.Body == nil {
			t.Error("Body is nil; a server-side request carries http.NoBody")
		}
		r.Header.Set("X-Handler-Touched", "yes")
		w.WriteHeader(http.StatusOK)
	})

	tr := &Transport{Handler: mux}
	req, err := http.NewRequest(http.MethodGet, "http://cortex/agents/team1/bot", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tok")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if got := req.Header.Get("X-Handler-Touched"); got != "" {
		t.Errorf("caller's header was modified: X-Handler-Touched = %q", got)
	}
	if req.RequestURI != "" {
		t.Errorf("caller's RequestURI was set to %q; a client request leaves it empty", req.RequestURI)
	}
	if req.Body != nil {
		t.Error("caller's Body was replaced")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q, want it preserved", got)
	}
	if req.PathValue("namespace") != "" {
		t.Error("caller's request gained path values from the mux")
	}
}

func TestRoundTripSendsBody(t *testing.T) {
	var (
		gotBody   string
		gotLength int64
		gotMethod string
	)
	tr := &Transport{Handler: handlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotLength, gotMethod = string(b), r.ContentLength, r.Method
		w.WriteHeader(http.StatusCreated)
	})}

	const payload = `{"name":"bot"}`
	req, err := http.NewRequest(http.MethodPost, "http://cortex/api/v1/agents",
		bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if gotBody != payload {
		t.Errorf("body = %q, want %q", gotBody, payload)
	}
	if gotLength != int64(len(payload)) {
		t.Errorf("ContentLength = %d, want %d", gotLength, len(payload))
	}
	if gotMethod != http.MethodPost {
		t.Errorf("Method = %q, want POST", gotMethod)
	}
}

// TestRoundTripCarriesContext verifies cancelling the command reaches the
// handler, which is what bounds serve's one outbound call (the agent-card fetch).
func TestRoundTripCarriesContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var handlerErr error
	tr := &Transport{Handler: handlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerErr = r.Context().Err()
		w.WriteHeader(http.StatusOK)
	})}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cortex/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if !errors.Is(handlerErr, context.Canceled) {
		t.Errorf("handler context error = %v, want context.Canceled", handlerErr)
	}
}

// TestRoundTripNonSuccessIsNotAnError is the crux of "same handlers, same
// results". serve answers 500 UNIMPLEMENTED for most routes; that has to reach
// the caller as the response it is, and surface from apiclient as the same
// *apiclient.StatusError the daemon would produce. Flattening it into a transport
// error here would make the two paths report different kinds of failure.
func TestRoundTripNonSuccessIsNotAnError(t *testing.T) {
	const body = `{"error":"UNIMPLEMENTED"}`
	tr := &Transport{Handler: handlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, body)
	})}

	req, _ := http.NewRequest(http.MethodGet, "http://cortex/api/v1/auth/status", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned an error for a 500 response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", resp.StatusCode)
	}

	// Through the real client, the same response must be a *StatusError.
	client := &apiclient.Client{
		BaseURL:    "http://cortex/api/v1/",
		HTTPClient: &http.Client{Transport: tr},
	}
	_, err = client.GetAuthStatus(context.Background())
	if err == nil {
		t.Fatal("GetAuthStatus succeeded, want an error")
	}
	var statusErr *apiclient.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error is %T (%v), want *apiclient.StatusError", err, err)
	}
	if statusErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", statusErr.StatusCode)
	}
}

func TestRoundTripIsConcurrencySafe(t *testing.T) {
	tr := &Transport{Handler: handlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, r.URL.Path)
	})}

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/api/v1/agents/%d", i)
			req, err := http.NewRequest(http.MethodGet, "http://cortex"+path, nil)
			if err != nil {
				t.Errorf("NewRequest: %v", err)
				return
			}
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Errorf("RoundTrip: %v", err)
				return
			}
			defer resp.Body.Close()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Errorf("reading body: %v", err)
				return
			}
			// Each goroutine must get its own path back, not another's.
			if string(b) != path {
				t.Errorf("body = %q, want %q", b, path)
			}
		}(i)
	}
	wg.Wait()
}

func TestMountPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		server  string
		want    string
		wantErr bool
	}{
		// The four spellings a context server can take. All must yield a mount
		// path the URLs apiclient builds from the same string resolve into.
		{name: "path with trailing slash", server: "http://localhost:9097/api/v1/", want: "/api/v1/"},
		{name: "path without trailing slash", server: "http://localhost:9097/api/v1", want: "/api/v1/"},
		{name: "no path", server: "http://localhost:9097", want: "/"},
		{name: "root path", server: "http://localhost:9097/", want: "/"},

		{name: "nested path", server: "http://localhost:9097/base/api/v1/", want: "/base/api/v1/"},
		{name: "https", server: "https://localhost:9097/api/v1/", want: "/api/v1/"},
		{name: "empty", server: "", wantErr: true},
		{name: "malformed", server: "http://[::1", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mountPath(tc.server)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mountPath(%q) = %q, want an error", tc.server, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mountPath(%q): %v", tc.server, err)
			}
			if got != tc.want {
				t.Errorf("mountPath(%q) = %q, want %q", tc.server, got, tc.want)
			}
		})
	}
}

// writeInstance records one instance under the isolated config home, so the real
// serve handlers have something to list.
//
// serve's lister/getter are package-private, so they cannot be stubbed from here;
// real records on disk are the only way to drive those handlers, and are also the
// more faithful test.
func writeInstance(t *testing.T, namespace, name string, proto instances.Protocol) {
	t.Helper()
	if _, err := instances.Create(instances.Instance{
		Name:            name,
		Namespace:       namespace,
		InboundAddr:     "127.0.0.1:0",
		InboundProtocol: proto,
	}); err != nil {
		t.Fatalf("creating instance %s/%s: %v", namespace, name, err)
	}
}

// TestNewServesRealHandlers is the integration check: a client built by New must
// reach the real serve routes rather than 404, for both a mounted and a
// root-mounted server. A mount-path derivation that disagrees with apiclient's
// URL building would show up here as a 404 on every call.
func TestNewServesRealHandlers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server string
	}{
		{name: "mounted at /api/v1/", server: "http://localhost:9097/api/v1/"},
		{name: "mounted at the root", server: "http://localhost:9097"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			writeInstance(t, "team1", "bot", instances.ProtocolA2A)
			writeInstance(t, "team2", "shell", instances.ProtocolMCP)

			httpClient, err := New(&config.Context{Name: "cortex", Server: tc.server})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			client := &apiclient.Client{BaseURL: tc.server, HTTPClient: httpClient}
			ctx := context.Background()

			agents, err := client.ListAgents(ctx, "team1")
			if err != nil {
				t.Fatalf("ListAgents: %v", err)
			}
			if len(agents.Items) != 1 || agents.Items[0].Name != "bot" {
				t.Errorf("agents = %+v, want just bot", agents.Items)
			}

			tools, err := client.ListTools(ctx, "team2")
			if err != nil {
				t.Fatalf("ListTools: %v", err)
			}
			if len(tools.Items) != 1 || tools.Items[0].Name != "shell" {
				t.Errorf("tools = %+v, want just shell", tools.Items)
			}

			// The namespaces come from the directories on disk, which is the one
			// intended difference from the daemon's --namespaces default.
			ns, err := client.ListNamespaces(ctx, true)
			if err != nil {
				t.Fatalf("ListNamespaces: %v", err)
			}
			if len(ns.Namespaces) != 2 || ns.Namespaces[0] != "team1" || ns.Namespaces[1] != "team2" {
				t.Errorf("namespaces = %v, want [team1 team2]", ns.Namespaces)
			}

			// A detail route, which is what needs the mux's path values.
			agent, err := client.GetAgent(ctx, "team1", "bot")
			if err != nil {
				t.Fatalf("GetAgent: %v", err)
			}
			if agent.Metadata.Name != "bot" {
				t.Errorf("agent name = %q, want bot", agent.Metadata.Name)
			}
		})
	}
}

// TestNewReportsUnimplementedRoutes pins that the routes serve does not implement
// stay unimplemented here. Making them work is a change to serve, not something
// this transport should paper over.
func TestNewReportsUnimplementedRoutes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const server = "http://localhost:9097/api/v1/"
	httpClient, err := New(&config.Context{Name: "cortex", Server: server})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := &apiclient.Client{BaseURL: server, HTTPClient: httpClient}

	_, err = client.GetAuthStatus(context.Background())
	if err == nil {
		t.Fatal("GetAuthStatus succeeded; serve does not implement /auth/status")
	}
	var statusErr *apiclient.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error is %T (%v), want *apiclient.StatusError", err, err)
	}
	if statusErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500 as the daemon returns", statusErr.StatusCode)
	}
}

// TestNewOnEmptyMachine verifies a machine where nothing has ever run is not an
// error: instances.Namespaces reports nil for a missing base directory, and serve
// renders that as an empty list rather than null.
func TestNewOnEmptyMachine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const server = "http://localhost:9097/api/v1/"
	httpClient, err := New(&config.Context{Name: "cortex", Server: server})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := &apiclient.Client{BaseURL: server, HTTPClient: httpClient}

	ns, err := client.ListNamespaces(context.Background(), true)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns.Namespaces) != 0 {
		t.Errorf("namespaces = %v, want none", ns.Namespaces)
	}

	agents, err := client.ListAgents(context.Background(), "")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents.Items) != 0 {
		t.Errorf("agents = %+v, want none", agents.Items)
	}
}

func TestNewRejectsContextWithNoServer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	client, err := New(&config.Context{Name: "cortex"})
	if err == nil {
		t.Fatalf("New returned %#v, want an error for a context with no server", client)
	}
	if client != nil {
		t.Errorf("client = %#v, want nil alongside the error", client)
	}
}

// TestNewBindsNothing verifies building the handler does not occupy the port in
// the context's server, which is the property that lets a command run while a
// daemon is also using it.
func TestNewBindsNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Hold a real listener on a port, then build a client pointed at it. If New
	// bound anything, this would fail rather than coexist.
	srv := httptest.NewServer(handlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := New(&config.Context{Name: "cortex", Server: srv.URL + "/api/v1/"}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// TestProbesServedInProcess verifies the health and readiness probes answer
// through the transport, at the site root and under the mount path, exactly as
// they do through the daemon. A root-mounted server has no prefix, so only the
// two root paths exist there.
func TestProbesServedInProcess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server string
		path   string
		want   int
		status string
	}{
		{"prefixed root health", "http://localhost:9097/api/v1/", "/health", http.StatusOK, "healthy"},
		{"prefixed root ready", "http://localhost:9097/api/v1/", "/ready", http.StatusOK, "ready"},
		{"prefixed mount health", "http://localhost:9097/api/v1/", "/api/v1/health", http.StatusOK, "healthy"},
		{"prefixed mount ready", "http://localhost:9097/api/v1/", "/api/v1/ready", http.StatusOK, "ready"},
		{"root-mounted health", "http://localhost:9097", "/health", http.StatusOK, "healthy"},
		{"root-mounted ready", "http://localhost:9097", "/ready", http.StatusOK, "ready"},
		// No API is mounted under /api/v1 on a root-mounted server, so this 404s
		// rather than falling back to the root probe.
		{"root-mounted has no prefix", "http://localhost:9097", "/api/v1/health", http.StatusNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())

			client, err := New(&config.Context{Name: "cortex", Server: tc.server})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			resp, err := client.Get("http://localhost:9097" + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.status == "" {
				return
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", ct)
			}
			// DisallowUnknownFields so an extra field in the wire shape is caught
			// here rather than confusing a probe consumer.
			dec := json.NewDecoder(resp.Body)
			dec.DisallowUnknownFields()
			var got serve.Health
			if err := dec.Decode(&got); err != nil {
				t.Fatalf("decoding %s: %v", tc.path, err)
			}
			if got.Status != tc.status {
				t.Errorf("status = %q, want %q", got.Status, tc.status)
			}
		})
	}
}
