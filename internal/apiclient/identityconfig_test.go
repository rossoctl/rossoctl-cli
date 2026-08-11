package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestGetAgentIdentityConfig verifies the request path and that the mode and both
// pipeline stages are decoded.
func TestGetAgentIdentityConfig(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"mode": "proxy-sidecar",
			"pipeline": {
				"inbound": {"plugins": [{"name": "jwt-validation"}]},
				"outbound": {"plugins": [{"name": "token-exchange"}]}
			}
		}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	cfg, err := c.GetAgentIdentityConfig(context.Background(), "team1", "orders")
	if err != nil {
		t.Fatalf("GetAgentIdentityConfig: %v", err)
	}

	if want := "/api/v1/agents/team1/orders/identity-config"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if cfg.Mode != "proxy-sidecar" {
		t.Errorf("Mode = %q, want proxy-sidecar", cfg.Mode)
	}
	if len(cfg.Pipeline.Inbound.Plugins) != 1 || cfg.Pipeline.Inbound.Plugins[0].Name != "jwt-validation" {
		t.Errorf("inbound plugins = %+v", cfg.Pipeline.Inbound.Plugins)
	}
	if len(cfg.Pipeline.Outbound.Plugins) != 1 || cfg.Pipeline.Outbound.Plugins[0].Name != "token-exchange" {
		t.Errorf("outbound plugins = %+v", cfg.Pipeline.Outbound.Plugins)
	}
}

// TestGetAgentIdentityConfigEscapesPath verifies namespace and name are escaped,
// so a name with a slash cannot reach a different endpoint than the one asked for.
func TestGetAgentIdentityConfigEscapesPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"waypoint"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	if _, err := c.GetAgentIdentityConfig(context.Background(), "team1", "a/b"); err != nil {
		t.Fatalf("GetAgentIdentityConfig: %v", err)
	}
	if want := "/api/v1/agents/team1/a%2Fb/identity-config"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// TestPluginEntryAcceptsBothSpellings verifies a plugin entry decodes from either
// a bare name or a full object.
//
// authlib's own type implements UnmarshalYAML but not UnmarshalJSON, so the bare
// form — which its YAML accepts — is not decodable as JSON without this. A server
// marshaling a loaded config emits objects, but one building the JSON itself may
// not, and that failure would take the whole response down.
func TestPluginEntryAcceptsBothSpellings(t *testing.T) {
	tests := []struct {
		name        string
		json        string
		wantName    string
		wantID      string
		wantPolicy  string
		wantNoConfg bool
	}{
		{
			name:        "bare name",
			json:        `"jwt-validation"`,
			wantName:    "jwt-validation",
			wantNoConfg: true,
		},
		{
			name:        "object without config",
			json:        `{"name":"ibac","on_error":"off"}`,
			wantName:    "ibac",
			wantPolicy:  "off",
			wantNoConfg: true,
		},
		{
			name:       "object with id and config",
			json:       `{"name":"opa","id":"opa-main","on_error":"observe","config":{"fail_open":false}}`,
			wantName:   "opa",
			wantID:     "opa-main",
			wantPolicy: "observe",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p PluginEntry
			if err := json.Unmarshal([]byte(tc.json), &p); err != nil {
				t.Fatalf("decoding %s: %v", tc.json, err)
			}
			if p.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", p.Name, tc.wantName)
			}
			if p.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", p.ID, tc.wantID)
			}
			if string(p.OnError) != tc.wantPolicy {
				t.Errorf("OnError = %q, want %q", p.OnError, tc.wantPolicy)
			}
			if got := len(p.Config) == 0; got != tc.wantNoConfg {
				t.Errorf("config empty = %v, want %v (config=%s)", got, tc.wantNoConfg, p.Config)
			}
		})
	}
}

// TestPluginEntryRejectsMalformed verifies a plugin entry that is neither a name
// nor an object is an error rather than being silently decoded as blank.
func TestPluginEntryRejectsMalformed(t *testing.T) {
	for _, in := range []string{`42`, `[1,2]`, `true`} {
		var p PluginEntry
		if err := json.Unmarshal([]byte(in), &p); err == nil {
			t.Errorf("decoding %s should fail, got %+v", in, p)
		}
	}
}

// TestPluginEntryConfigIsUninterpreted verifies a plugin's config survives a
// decode/encode round trip with its keys intact, including ones no plugin in this
// build knows about. The framework leaves that subtree to the plugin, so the
// client must not reshape it.
func TestPluginEntryConfigIsUninterpreted(t *testing.T) {
	const in = `{"name":"custom","config":{"unknown_key":{"deep":[1,2,{"x":null}]}}}`

	var p PluginEntry
	if err := json.Unmarshal([]byte(in), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var want, got any
	if err := json.Unmarshal([]byte(in), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round trip changed the entry:\n in: %s\nout: %s", in, out)
	}
}

// TestIdentityConfigPreservesUnknownFields verifies top-level blocks this client
// does not model survive a round trip, so --json stays a faithful view of the
// response rather than of the client's model of it.
func TestIdentityConfigPreservesUnknownFields(t *testing.T) {
	const in = `{
		"mode": "proxy-sidecar",
		"listener": {"reverse_proxy_addr": ":8080"},
		"session": {"enabled": true},
		"future_block": {"added": "later"},
		"pipeline": {
			"inbound": {"plugins": [{"name": "jwt-validation"}]},
			"outbound": {"plugins": []}
		}
	}`

	var cfg AgentIdentityConfig
	if err := json.Unmarshal([]byte(in), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	for _, key := range []string{"mode", "pipeline", "listener", "session", "future_block"} {
		if _, ok := got[key]; !ok {
			t.Errorf("round trip dropped %q:\n%s", key, out)
		}
	}
	// The unknown blocks must keep their contents, not just their keys.
	future, ok := got["future_block"].(map[string]any)
	if !ok || future["added"] != "later" {
		t.Errorf("future_block lost its contents: %v", got["future_block"])
	}
}

// TestIdentityConfigEmptyPipeline verifies an absent pipeline decodes to empty
// stages rather than failing, since a zero value is what callers render.
func TestIdentityConfigEmptyPipeline(t *testing.T) {
	var cfg AgentIdentityConfig
	if err := json.Unmarshal([]byte(`{"mode":"waypoint"}`), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Mode != "waypoint" {
		t.Errorf("Mode = %q", cfg.Mode)
	}
	if len(cfg.Pipeline.Inbound.Plugins) != 0 || len(cfg.Pipeline.Outbound.Plugins) != 0 {
		t.Errorf("want empty stages, got %+v", cfg.Pipeline)
	}
}

// TestGetAgentIdentityConfigServerError verifies a non-2xx becomes a StatusError
// carrying the code, so the command layer can react to the status rather than to
// message text.
func TestGetAgentIdentityConfigServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"not found"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/api/v1/"}
	_, err := c.GetAgentIdentityConfig(context.Background(), "team1", "orders")
	if err == nil {
		t.Fatal("a 404 should be an error")
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *StatusError", err)
	}
	if se.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", se.StatusCode)
	}
}
