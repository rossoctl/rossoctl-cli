package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubCardFetcher replaces the card fetch with a canned answer, so these tests do
// not need a live agent listening on the fixture's inbound address.
func stubCardFetcher(t *testing.T, body string, status int, err error) *string {
	t.Helper()
	saved := cardFetcher
	var requested string
	cardFetcher = func(_ context.Context, cardURL string) ([]byte, int, error) {
		requested = cardURL
		return []byte(body), status, err
	}
	t.Cleanup(func() { cardFetcher = saved })
	return &requested
}

// a2aCard is a card as a v0.3 A2A server serves one: url at the top level,
// streaming nested under capabilities. The declared host is 0.0.0.0, which is what
// an agent bound to all interfaces reports and what makes the rewrite necessary.
const a2aCard = `{
  "name": "Weather Agent",
  "description": "Answers weather questions",
  "version": "1.2.3",
  "url": "http://0.0.0.0:9999/a2a",
  "protocolVersion": "0.3.0",
  "preferredTransport": "JSONRPC",
  "capabilities": {"streaming": true},
  "skills": [
    {"id": "forecast", "name": "Forecast", "description": "gets weather",
     "tags": ["weather"], "examples": ["what is it in NYC?"]}
  ]
}`

// getCard requests the agent-card endpoint and decodes the response.
func getCard(t *testing.T, ts *httptest.Server, path string) map[string]any {
	t.Helper()
	res, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", path, res.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// cardStatus requests the endpoint and returns just the status and detail message.
func cardStatus(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	res, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		Detail string `json:"detail"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body.Detail
}

// TestAgentCardFetchesFromTheRecordedAddress verifies the fetch targets the
// instance's own inbound address and the well-known path, which is the whole
// reason this endpoint consults the record at all.
func TestAgentCardFetchesFromTheRecordedAddress(t *testing.T) {
	stubGetter(t, mixedInstances())
	requested := stubCardFetcher(t, a2aCard, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	getCard(t, ts, "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")

	// swift-falcon-0001's recorded inbound address, plus the well-known path.
	if want := "http://127.0.0.1:8080/.well-known/agent-card.json"; *requested != want {
		t.Errorf("fetched %q, want %q", *requested, want)
	}
}

// TestAgentCardConvertsToTheBackendShape verifies the response is the backend's
// reshaped AgentCardResponse rather than the spec card the agent served: streaming
// flattened out of capabilities, and the skill keys the backend emits.
func TestAgentCardConvertsToTheBackendShape(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, a2aCard, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	got := getCard(t, ts, "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")

	if got["name"] != "Weather Agent" {
		t.Errorf("name = %v, want %q", got["name"], "Weather Agent")
	}
	if got["version"] != "1.2.3" {
		t.Errorf("version = %v, want %q", got["version"], "1.2.3")
	}
	if got["description"] != "Answers weather questions" {
		t.Errorf("description = %v", got["description"])
	}
	// Flattened to the top level, where the agent nested it under capabilities.
	if got["streaming"] != true {
		t.Errorf("streaming = %v, want true (the card declares capabilities.streaming)", got["streaming"])
	}
	// The nested capabilities object itself is not forwarded: this is the backend's
	// simplified shape, not the spec card.
	if _, present := got["capabilities"]; present {
		t.Errorf("capabilities should not be forwarded, got %v", got["capabilities"])
	}

	skills, ok := got["skills"].([]any)
	if !ok || len(skills) != 1 {
		t.Fatalf("skills = %v, want one entry", got["skills"])
	}
	skill, ok := skills[0].(map[string]any)
	if !ok {
		t.Fatalf("skill = %v, want an object", skills[0])
	}
	for key, want := range map[string]any{
		"id":          "forecast",
		"name":        "Forecast",
		"description": "gets weather",
	} {
		if skill[key] != want {
			t.Errorf("skill.%s = %v, want %v", key, skill[key], want)
		}
	}
	if ex, _ := skill["examples"].([]any); len(ex) != 1 || ex[0] != "what is it in NYC?" {
		t.Errorf("skill.examples = %v", skill["examples"])
	}
	// Tags are carried even though the backend's own conversion drops them; the
	// CLI's renderer shows them, and its skills field is an open dict.
	if tags, _ := skill["tags"].([]any); len(tags) != 1 || tags[0] != "weather" {
		t.Errorf("skill.tags = %v, want [weather]", skill["tags"])
	}
}

// TestAgentCardURLPointsAtTheInstance verifies the declared host is replaced with
// the address this host reaches the agent on, while the path is kept.
//
// This is the point of the rewrite: an agent bound to all interfaces declares
// 0.0.0.0, which resolves nowhere useful for a caller.
func TestAgentCardURLPointsAtTheInstance(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, a2aCard, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	got := getCard(t, ts, "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")

	if want := "http://127.0.0.1:8080/a2a"; got["url"] != want {
		t.Errorf("url = %v, want %q (host replaced, path kept)", got["url"], want)
	}
}

// TestRewriteCardHost covers the URL surgery directly, where the cases are easier
// to enumerate than through a served response.
func TestRewriteCardHost(t *testing.T) {
	const addr = "127.0.0.1:8080"

	for _, tc := range []struct {
		name, declared, want string
	}{
		{"host and port replaced, path kept", "http://0.0.0.0:9999/a2a", "http://127.0.0.1:8080/a2a"},
		{"no path stays pathless", "http://0.0.0.0:9999", "http://127.0.0.1:8080"},
		{"trailing slash kept", "http://0.0.0.0:9999/", "http://127.0.0.1:8080/"},
		{"nested path kept", "http://agent.svc:80/deep/path", "http://127.0.0.1:8080/deep/path"},
		{"query and fragment kept", "http://a:1/p?x=1#f", "http://127.0.0.1:8080/p?x=1#f"},
		{"portless host replaced", "http://agent.internal/a2a", "http://127.0.0.1:8080/a2a"},

		// An https card would send a caller to a TLS port that is not listening: the
		// record's inbound address is a plain listener.
		{"https downgraded to http", "https://agent.svc/a2a", "http://127.0.0.1:8080/a2a"},

		// Userinfo belongs to the authority being replaced, so it goes with it.
		{"userinfo dropped", "http://user:pw@0.0.0.0:9999/a2a", "http://127.0.0.1:8080/a2a"},

		// Degenerate inputs fall back to the plain address rather than to something
		// unusable.
		{"empty falls back", "", "http://127.0.0.1:8080"},
		{"hostless falls back", "/just/a/path", "http://127.0.0.1:8080"},
		{"unparseable falls back", "http://[::1", "http://127.0.0.1:8080"},
	} {
		if got := rewriteCardHost(tc.declared, addr); got != tc.want {
			t.Errorf("%s: rewriteCardHost(%q) = %q, want %q", tc.name, tc.declared, got, tc.want)
		}
	}
}

// TestAgentCardKeepsPathWhenTransportIsAbsent is the case the upstream v0.3 parser
// gets wrong on its own: without preferredTransport it reconstructs no interface
// and the URL is lost. Reading the flat url is what keeps the endpoint here.
func TestAgentCardKeepsPathWhenTransportIsAbsent(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, `{"name":"Minimal","version":"1","url":"http://0.0.0.0:9999/a2a",
	  "capabilities":{"streaming":false}}`, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	got := getCard(t, ts, "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")
	if want := "http://127.0.0.1:8080/a2a"; got["url"] != want {
		t.Errorf("url = %v, want %q; a card without preferredTransport must not lose its path",
			got["url"], want)
	}
}

// TestAgentCardDefaultsMirrorTheBackend verifies the fallbacks match the Python
// handler's: a missing version reads "unknown", a missing description is null, and
// skills default to an empty array rather than null.
func TestAgentCardDefaultsMirrorTheBackend(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, `{"name":"Bare","url":"http://0.0.0.0:1/"}`, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	res, err := ts.Client().Get(ts.URL + "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(raw["version"]); got != `"unknown"` {
		t.Errorf("version = %s, want \"unknown\" for a card that declares none", got)
	}
	if got := string(raw["description"]); got != "null" {
		t.Errorf("description = %s, want null for a card that declares none", got)
	}
	// An empty array, not null: the backend's field defaults to [].
	if got := string(raw["skills"]); got != "[]" {
		t.Errorf("skills = %s, want [] for a card with no skills", got)
	}
	if got := string(raw["streaming"]); got != "false" {
		t.Errorf("streaming = %s, want false", got)
	}
}

// TestAgentCardMissingInstanceIsNotFound verifies this endpoint 404s exactly where
// the detail endpoint does, so a caller is not told an instance exists on one
// endpoint and not another.
func TestAgentCardMissingInstanceIsNotFound(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, a2aCard, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	status, detail := cardStatus(t, ts, "/api/v1/chat/recorded1/no-such-agent/agent-card")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if detail == "" {
		t.Error("a 404 should say what was not found")
	}
}

// TestAgentCardMCPInstanceIsNotFound verifies an mcp instance has no agent card. A
// card is an A2A concept, and the agents endpoints already report mcp instances as
// absent.
func TestAgentCardMCPInstanceIsNotFound(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, a2aCard, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	// calm-harbor-0002 is the mcp fixture.
	status, _ := cardStatus(t, ts, "/api/v1/chat/recorded1/calm-harbor-0002/agent-card")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an mcp instance", status)
	}
}

// TestAgentCardUnreachableAgentIsUnavailable verifies a connection failure is a
// 503 naming the address, not a 500: the instance exists and the agent is down,
// which is the agent's state rather than a fault in this server.
func TestAgentCardUnreachableAgentIsUnavailable(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, "", 0, fmt.Errorf("connection refused"))
	ts := newTestServer(t, "/api/v1")

	status, detail := cardStatus(t, ts, "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
	if !strings.Contains(detail, "127.0.0.1:8080") {
		t.Errorf("detail = %q, should name the address that could not be reached", detail)
	}
}

// TestAgentCardPassesThroughAgentStatus verifies the agent's own error status is
// forwarded rather than flattened, so a caller can tell which hop refused. An
// agent answering 404 at the well-known path is not an A2A agent.
func TestAgentCardPassesThroughAgentStatus(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, `{"detail":"not found"}`, http.StatusNotFound, nil)
	ts := newTestServer(t, "/api/v1")

	status, detail := cardStatus(t, ts, "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want the agent's own 404", status)
	}
	if !strings.Contains(detail, "404") {
		t.Errorf("detail = %q, should report the status the agent returned", detail)
	}
}

// TestAgentCardUnreadableBodyIsBadGateway verifies a reachable agent serving
// something that is not a card is a 502, distinguishing "bad answer" from the 503
// that means "no answer".
func TestAgentCardUnreadableBodyIsBadGateway(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, "this is not json", http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	status, detail := cardStatus(t, ts, "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if !strings.Contains(detail, "127.0.0.1:8080") {
		t.Errorf("detail = %q, should name the agent that served it", detail)
	}
}

// TestAgentCardWithoutInboundAddressIsUnavailable verifies an instance with no
// inbound listener is a 503 rather than a 404: the instance exists, so reporting
// it absent would contradict the detail endpoint.
func TestAgentCardWithoutInboundAddressIsUnavailable(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, a2aCard, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	// keen-ridge-0003 is recorded with no inbound address.
	status, detail := cardStatus(t, ts, "/api/v1/chat/recorded2/keen-ridge-0003/agent-card")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for an instance with no inbound listener", status)
	}
	if !strings.Contains(detail, "keen-ridge-0003") {
		t.Errorf("detail = %q, should name the instance", detail)
	}
}
