package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// agentCardBody mimics the backend's AgentCardResponse: every field the response
// model declares, with two skills and a multi-line description.
//
// Note streaming is a top-level boolean. The A2A card an agent serves nests it
// under "capabilities", but the backend flattens it before answering, so that is
// the shape a client sees.
const agentCardBody = `{
	"name": "Orders Agent",
	"description": "Places and tracks orders.\nSupports cancellation.",
	"version": "1.2.3",
	"url": "http://orders.team1.svc:8000/",
	"streaming": true,
	"skills": [
		{
			"id": "place-order",
			"name": "Place Order",
			"description": "Creates a new order.",
			"tags": ["orders", "write"],
			"examples": ["Order two widgets", "Buy a hat"]
		},
		{
			"id": "track-order",
			"name": "Track Order",
			"description": "Reports an order's status.",
			"tags": ["orders"]
		}
	]
}`

// newAgentCardServer serves the card path with the given body and status, and
// the namespace list set-context validates against.
func newAgentCardServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case "/api/v1/chat/team1/orders/agent-card":
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAgentsCardText(t *testing.T) {
	isolateHome(t)
	srv := newAgentCardServer(t, agentCardBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "card", "orders")
	if err != nil {
		t.Fatalf("agents card: %v", err)
	}

	for _, want := range []string{
		"Orders Agent",
		"Basic Information",
		"Version:", "1.2.3",
		"URL:", "http://orders.team1.svc:8000/",
		"Streaming:", "Enabled",
		"Description",
		"Places and tracks orders.",
		"Supports cancellation.",
		"Skills",
		"Place Order", "orders, write",
		"Creates a new order.",
		"Examples:", "Order two widgets", "Buy a hat",
		"Track Order",
		"Reports an order's status.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("card text missing %q:\n%s", want, out)
		}
	}

	// It must not be raw JSON.
	if strings.Contains(out, "\"skills\"") {
		t.Errorf("text output unexpectedly contains raw JSON:\n%s", out)
	}
}

// TestAgentsCardJSON verifies --json prints the server's card, not a rendering
// of it.
func TestAgentsCardJSON(t *testing.T) {
	isolateHome(t)
	srv := newAgentCardServer(t, agentCardBody, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "card", "orders", "--json")
	if err != nil {
		t.Fatalf("agents card --json: %v", err)
	}
	var decoded struct {
		Name   string `json:"name"`
		Skills []struct {
			ID string `json:"id"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if decoded.Name != "Orders Agent" {
		t.Errorf("name = %q, want Orders Agent", decoded.Name)
	}
	if len(decoded.Skills) != 2 || decoded.Skills[0].ID != "place-order" {
		t.Errorf("skills not carried through: %+v", decoded.Skills)
	}
	// The rendered section headings must not be there.
	if strings.Contains(out, "Basic Information") {
		t.Errorf("--json output should not be rendered as text:\n%s", out)
	}
}

// TestAgentsCardMinimal verifies a card carrying only the fields the A2A spec
// requires renders without blank rows for the ones it omits.
func TestAgentsCardMinimal(t *testing.T) {
	isolateHome(t)
	body := `{"name": "bare", "version": "0.1", "url": "http://x/"}`
	srv := newAgentCardServer(t, body, 0)
	setupAgentGetContext(t, srv)

	out, err := execute(t, "agents", "card", "orders")
	if err != nil {
		t.Fatalf("agents card: %v", err)
	}
	for _, want := range []string{"bare", "Version:", "0.1", "URL:", "http://x/"} {
		if !strings.Contains(out, want) {
			t.Errorf("minimal card missing %q:\n%s", want, out)
		}
	}
	// Absent optional fields get no section at all, rather than an empty one.
	for _, absent := range []string{"Description", "Skills"} {
		if strings.Contains(out, absent) {
			t.Errorf("%q should be omitted from a minimal card:\n%s", absent, out)
		}
	}
	// Streaming is the exception: false is an answer, so the row stays.
	if !strings.Contains(out, "Streaming:") || !strings.Contains(out, "Disabled") {
		t.Errorf("Streaming should be reported as Disabled even when absent:\n%s", out)
	}
}

// TestAgentsCardNotRunning verifies the command fails, rather than printing an
// empty card, when the agent is not up to serve one. The backend answers 502 or
// 404 in that case; either way there is no card, and reporting one would be a
// fabrication.
func TestAgentsCardNotRunning(t *testing.T) {
	isolateHome(t)
	for _, status := range []int{http.StatusNotFound, http.StatusBadGateway} {
		srv := newAgentCardServer(t, `{"detail":"agent is not running"}`, status)
		setupAgentGetContext(t, srv)

		out, err := execute(t, "agents", "card", "orders")
		if err == nil {
			t.Errorf("agents card should fail when the server answers %d, got:\n%s", status, out)
		}
	}
}

// TestAgentsCardNamespaceOverride verifies --namespace selects the namespace in
// the card path, which lives under /chat rather than /agents.
func TestAgentsCardNamespaceOverride(t *testing.T) {
	isolateHome(t)

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/namespaces" {
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))
			return
		}
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"name":"orders","version":"1","url":"http://x/","capabilities":{}}`))
	}))
	t.Cleanup(srv.Close)
	setupAgentGetContext(t, srv)

	if _, err := execute(t, "agents", "--namespace", "team2", "card", "orders"); err != nil {
		t.Fatalf("agents card: %v", err)
	}
	if gotPath != "/api/v1/chat/team2/orders/agent-card" {
		t.Errorf("requested path = %q, want /api/v1/chat/team2/orders/agent-card", gotPath)
	}
}
