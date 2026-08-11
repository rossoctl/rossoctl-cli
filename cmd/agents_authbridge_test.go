package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// identityConfigBody mimics the backend GET
// /agents/{ns}/{name}/identity-config response: a proxy-sidecar config whose
// inbound stage exercises every plugin-entry variation (a bare name, an entry
// with an id and a nested config, and one disabled via on_error) and whose
// outbound stage carries a single configured plugin.
//
// The top-level "listener" block is deliberately present and is not something
// this command renders: it stands in for the parts of AuthBridge's config that
// --json must still reproduce.
const identityConfigBody = `{
	"mode": "proxy-sidecar",
	"listener": {"reverse_proxy_addr": ":8080", "forward_proxy_addr": ":8081"},
	"pipeline": {
		"inbound": {
			"plugins": [
				{"name": "jwt-validation"},
				{
					"name": "opa",
					"id": "opa-main",
					"on_error": "observe",
					"config": {"policy_url": "http://opa:8181/v1/data/authz", "fail_open": false}
				},
				{"name": "ibac", "on_error": "off"}
			]
		},
		"outbound": {
			"plugins": [
				{"name": "token-exchange", "config": {"audience": "https://api.example.com"}}
			]
		}
	}
}`

// newIdentityConfigServer serves the identity-config path with the given body
// and status, plus the namespace list set-context validates against.
func newIdentityConfigServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))
		case "/api/v1/agents/team1/orders/identity-config",
			"/api/v1/agents/team2/orders/identity-config":
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAgentsAuthbridgeText verifies the default rendering reports the mode and
// both stages' plugins with their configurations.
func TestAgentsAuthbridgeText(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, identityConfigBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("agents authbridge: %v\noutput:\n%s", err, out)
	}

	for _, want := range []string{
		"proxy-sidecar",
		"Inbound Plugins",
		"Outbound Plugins",
		"jwt-validation",
		"opa",
		"ibac",
		"token-exchange",
		// Plugin configuration, not just plugin names.
		"policy_url",
		"http://opa:8181/v1/data/authz",
		"audience",
		"https://api.example.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestAgentsAuthbridgeStageOrdering verifies each plugin is reported under the
// stage it belongs to. A renderer that printed one combined list, or swapped the
// headings, would still contain every substring the test above checks.
func TestAgentsAuthbridgeStageOrdering(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, identityConfigBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("agents authbridge: %v", err)
	}

	inbound := strings.Index(out, "Inbound Plugins")
	outbound := strings.Index(out, "Outbound Plugins")
	if inbound < 0 || outbound < 0 {
		t.Fatalf("both stage headings should be present:\n%s", out)
	}
	if inbound > outbound {
		t.Errorf("inbound stage should be printed before outbound:\n%s", out)
	}

	// The inbound plugins must fall between the two headings, and the outbound
	// one after the second.
	for _, name := range []string{"jwt-validation", "opa", "ibac"} {
		if at := strings.Index(out, name); at < inbound || at > outbound {
			t.Errorf("%q should be listed under Inbound Plugins:\n%s", name, out)
		}
	}
	if at := strings.Index(out, "token-exchange"); at < outbound {
		t.Errorf("token-exchange should be listed under Outbound Plugins:\n%s", out)
	}
}

// TestAgentsAuthbridgeExecutionOrder verifies plugins are numbered in the order
// the server listed them, since that is the order the pipeline invokes them.
func TestAgentsAuthbridgeExecutionOrder(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, identityConfigBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("agents authbridge: %v", err)
	}

	for _, want := range []string{"1. jwt-validation", "2. opa", "3. ibac"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q, so execution order is not evident:\n%s", want, out)
		}
	}
}

// TestAgentsAuthbridgeReportsOnErrorPolicy verifies each plugin's on_error policy
// is reported, including the implicit default.
//
// This matters beyond cosmetics: "off" means the plugin is never dispatched and
// "observe" discards its verdict, so a listing that showed only names would
// present a disabled guardrail as if it were enforcing.
func TestAgentsAuthbridgeReportsOnErrorPolicy(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, identityConfigBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("agents authbridge: %v", err)
	}

	// An explicitly disabled plugin and a shadow-mode one.
	for _, want := range []string{"off", "observe"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should report the %q policy:\n%s", want, out)
		}
	}
	// An entry with no on_error is enforcing, and says so rather than showing
	// nothing — otherwise "not reported" and "enforcing" look identical.
	if !strings.Contains(out, "enforce") {
		t.Errorf("a plugin with no on_error should be reported as enforcing:\n%s", out)
	}
}

// TestAgentsAuthbridgeIDReported verifies an explicit plugin id is shown, since
// it is what distinguishes two entries of the same plugin.
func TestAgentsAuthbridgeIDReported(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, identityConfigBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("agents authbridge: %v", err)
	}
	if !strings.Contains(out, "opa-main") {
		t.Errorf("output missing the plugin id:\n%s", out)
	}
}

// TestAgentsAuthbridgeJSON verifies --json prints the response as JSON,
// preserving both plugin configuration and top-level blocks this command does
// not render.
func TestAgentsAuthbridgeJSON(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, identityConfigBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders", "--json")
	if err != nil {
		t.Fatalf("agents authbridge --json: %v\noutput:\n%s", err, out)
	}

	// It must be valid JSON, not the text rendering.
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput:\n%s", err, out)
	}
	if got["mode"] != "proxy-sidecar" {
		t.Errorf("mode = %v, want proxy-sidecar", got["mode"])
	}
	// A block the text rendering ignores must still survive, so --json stays a
	// faithful view of the response rather than of this command's model of it.
	if _, ok := got["listener"]; !ok {
		t.Errorf("--json dropped the listener block:\n%s", out)
	}

	// Plugin config must survive as structured JSON rather than as a string.
	pipeline, ok := got["pipeline"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline missing from --json output:\n%s", out)
	}
	inbound, ok := pipeline["inbound"].(map[string]any)
	if !ok {
		t.Fatalf("inbound missing from --json output:\n%s", out)
	}
	plugins, ok := inbound["plugins"].([]any)
	if !ok || len(plugins) != 3 {
		t.Fatalf("want 3 inbound plugins, got %v", inbound["plugins"])
	}
	opa, ok := plugins[1].(map[string]any)
	if !ok {
		t.Fatalf("second inbound plugin is not an object: %v", plugins[1])
	}
	cfg, ok := opa["config"].(map[string]any)
	if !ok {
		t.Fatalf("opa config should be a JSON object, got %v", opa["config"])
	}
	if cfg["policy_url"] != "http://opa:8181/v1/data/authz" {
		t.Errorf("policy_url = %v, want the configured URL", cfg["policy_url"])
	}
}

// TestAgentsAuthbridgeJSONNotText verifies --json suppresses the text rendering
// rather than printing both.
func TestAgentsAuthbridgeJSONNotText(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, identityConfigBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders", "--json")
	if err != nil {
		t.Fatalf("agents authbridge --json: %v", err)
	}
	if strings.Contains(out, "Inbound Plugins") {
		t.Errorf("--json should not print the text headings:\n%s", out)
	}
}

// TestAgentsAuthbridgeBarePluginName verifies a plugin written as a bare name is
// accepted.
//
// AuthBridge's YAML allows both a bare name and a full object, and normalizes the
// former when loading — so a server marshaling a loaded config emits objects. A
// server building this JSON directly may emit the bare string instead, and
// authlib's own type cannot decode that, which would fail the whole command
// including --json.
func TestAgentsAuthbridgeBarePluginName(t *testing.T) {
	isolateHome(t)
	const body = `{
		"mode": "waypoint",
		"pipeline": {
			"inbound": {"plugins": ["jwt-validation", {"name": "opa"}]},
			"outbound": {"plugins": ["token-exchange"]}
		}
	}`
	srv := newIdentityConfigServer(t, body, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("a bare plugin name should be accepted: %v\noutput:\n%s", err, out)
	}
	for _, want := range []string{"jwt-validation", "opa", "token-exchange"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestAgentsAuthbridgeEmptyPipeline verifies an empty stage is reported rather
// than omitted. A stage with no plugins means the listener has nothing to invoke
// — authlib warns about it at startup — so it must be visible, and distinguishable
// from a stage this command failed to render.
func TestAgentsAuthbridgeEmptyPipeline(t *testing.T) {
	isolateHome(t)
	const body = `{
		"mode": "envoy-sidecar",
		"pipeline": {"inbound": {"plugins": []}, "outbound": {"plugins": []}}
	}`
	srv := newIdentityConfigServer(t, body, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("agents authbridge: %v\noutput:\n%s", err, out)
	}
	for _, want := range []string{"Inbound Plugins", "Outbound Plugins", "(none)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "(none)") != 2 {
		t.Errorf("both empty stages should be reported as (none):\n%s", out)
	}
}

// TestAgentsAuthbridgeMissingPipeline verifies a response with no pipeline block
// at all still renders, rather than panicking on a zero value.
func TestAgentsAuthbridgeMissingPipeline(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, `{"mode": "waypoint"}`, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("agents authbridge: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "waypoint") {
		t.Errorf("output missing the mode:\n%s", out)
	}
}

// TestAgentsAuthbridgeUnsetMode verifies an absent mode is reported as unset
// rather than printed as an empty value. authlib requires a mode, so its absence
// is a real finding about the agent's configuration.
func TestAgentsAuthbridgeUnsetMode(t *testing.T) {
	isolateHome(t)
	const body = `{"pipeline": {"inbound": {"plugins": []}, "outbound": {"plugins": []}}}`
	srv := newIdentityConfigServer(t, body, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("agents authbridge: %v", err)
	}
	if !strings.Contains(out, "(unset)") {
		t.Errorf("an absent mode should be reported as unset:\n%s", out)
	}
}

// TestAgentsAuthbridgeUnparseableConfig verifies a plugin config that is not
// valid JSON is still shown. It is the kind of thing worth seeing, and the
// command exists to diagnose exactly this.
func TestAgentsAuthbridgeUnparseableConfig(t *testing.T) {
	isolateHome(t)
	// A config whose value is a bare number is valid JSON but not an object; a
	// plugin expecting a block would reject it, and the CLI should still print it.
	const body = `{
		"mode": "waypoint",
		"pipeline": {
			"inbound": {"plugins": [{"name": "opa", "config": 42}]},
			"outbound": {"plugins": []}
		}
	}`
	srv := newIdentityConfigServer(t, body, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err != nil {
		t.Fatalf("agents authbridge: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "42") {
		t.Errorf("an unusual plugin config should still be printed:\n%s", out)
	}
}

// TestAgentsAuthbridgeNamespaceOverride verifies the config is fetched from the
// namespace --namespace names, since that decides which agent "orders" means.
func TestAgentsAuthbridgeNamespaceOverride(t *testing.T) {
	isolateHome(t)
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/namespaces" {
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))
			return
		}
		_, _ = w.Write([]byte(identityConfigBody))
	}))
	t.Cleanup(srv.Close)
	setupAgentGetContext(t, srv)

	if _, err := execute(t, "agents", "--namespace", "team2", "authbridge", "orders"); err != nil {
		t.Fatalf("agents authbridge: %v", err)
	}

	want := "/api/v1/agents/team2/orders/identity-config"
	if !strings.Contains(strings.Join(paths, " "), want) {
		t.Errorf("want a request to %s; got %v", want, paths)
	}
}

// TestAgentsAuthbridgeRequiresName verifies the agent name is required.
func TestAgentsAuthbridgeRequiresName(t *testing.T) {
	isolateHome(t)
	if out, err := execute(t, "agents", "authbridge"); err == nil {
		t.Fatalf("agents authbridge without a name should fail, got:\n%s", out)
	}
}

// TestAgentsAuthbridgeServerError verifies a non-2xx response fails the command
// and surfaces the status, rather than printing an empty configuration.
func TestAgentsAuthbridgeServerError(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, `{"detail":"agent not found"}`, http.StatusNotFound)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "authbridge", "orders")
	if err == nil {
		t.Fatalf("a 404 should fail the command, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, want it to report the status", err)
	}
	if strings.Contains(out, "Inbound Plugins") {
		t.Errorf("a failed request should print no configuration:\n%s", out)
	}
}

// TestAgentsAuthbridgeMalformedResponse verifies a body that is not valid JSON is
// reported as a decode failure rather than rendered as an empty config.
func TestAgentsAuthbridgeMalformedResponse(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, `not json at all`, 0)
	setupAgentGetContext(t, srv)

	if out, err := execute(t, "agents", "authbridge", "orders"); err == nil {
		t.Fatalf("a malformed body should fail the command, got:\n%s", out)
	}
}

// TestAgentsAuthbridgeVerboseLogsRequest verifies --verbose reports the request
// on stderr while stdout keeps the rendered configuration.
func TestAgentsAuthbridgeVerboseLogsRequest(t *testing.T) {
	isolateHome(t)
	srv := newIdentityConfigServer(t, identityConfigBody, 0)
	setupAgentGetContext(t, srv)

	stdout, stderr, err := executeSplit(t, "agents", "authbridge", "orders", "--verbose")
	if err != nil {
		t.Fatalf("agents authbridge -v: %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "identity-config") {
		t.Errorf("stderr missing the request:\n%s", stderr)
	}
	if !strings.Contains(stdout, "Inbound Plugins") {
		t.Errorf("stdout missing the rendered config:\n%s", stdout)
	}
}
