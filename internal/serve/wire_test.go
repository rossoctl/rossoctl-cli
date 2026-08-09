package serve

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/agentapi"
	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

// TestDetailWireShapeMatchesClientType is the point of sharing agentapi: this
// server's output and the CLI's decoding are the same contract, so a field this
// server emits must be one the client reads.
//
// Decoding into the client's own type would pass even if the two had drifted,
// since encoding/json ignores unknown fields. So this decodes with
// DisallowUnknownFields, which turns any key the client's type does not declare
// into an error rather than a silently dropped value.
func TestDetailWireShapeMatchesClientType(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	res, err := http.Get(ts.URL + "/api/v1/agents/recorded1/swift-falcon-0001")
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var got apiclient.AgentDetail
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("server output does not fit the client's AgentDetail: %v", err)
	}

	// And the decode is not vacuous — the fields the client renders arrived.
	if got.Metadata.Name == "" || got.Metadata.Namespace == "" {
		t.Errorf("metadata did not survive the round trip: %+v", got.Metadata)
	}
	if got.ReadyStatus == "" || got.WorkloadType == "" {
		t.Errorf("status fields did not survive: %+v", got)
	}
	// A local instance has no Kubernetes Service. This is the one field whose Go
	// type changed when the two sides were merged (any -> *ServiceInfo), so it is
	// worth asserting it still arrives as absent rather than as a zero struct.
	if got.Service != nil {
		t.Errorf("Service should be null for a local instance, got %+v", got.Service)
	}
	// The timestamp crosses the wire as a *string, so its arrival proves the tag on
	// both sides agrees — a rename on either would decode as nil here, not error.
	if got.Metadata.CreationTimestamp == nil {
		t.Error("creationTimestamp did not survive the round trip")
	}
}

// TestAbsentCreationTimestampEmitsNull pins the wire form for a record written
// before the field existed.
//
// A nil *string decodes the same whether the server sent null or omitted the key
// entirely, so the Go-level test cannot tell those apart. The schema marks the
// field required-and-nullable, and a client decoding with DisallowUnknownFields is
// unbothered either way — but one of the two is what the schema says, so it is
// asserted on the raw bytes.
func TestAbsentCreationTimestampEmitsNull(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	// keen-ridge-0003 is the fixture with no recorded timestamp.
	res, err := http.Get(ts.URL + "/api/v1/agents/recorded2/keen-ridge-0003")
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var raw struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ts2, present := raw.Metadata["creationTimestamp"]
	if !present {
		t.Fatal(`"creationTimestamp" key should be present, as the schema marks it required`)
	}
	if string(ts2) != "null" {
		t.Errorf(`"creationTimestamp" = %s, want null for a record without one`, ts2)
	}
}

// TestServerAndClientShareOneType guards against the aliases being replaced by
// re-declared structs later. If someone gives either side its own copy, the
// identity below stops holding and this fails — which is the whole guarantee the
// agentapi package exists to provide.
func TestServerAndClientShareOneType(t *testing.T) {
	for _, tc := range []struct {
		name           string
		server, client any
	}{
		{"detail", ResourceDetail{}, apiclient.AgentDetail{}},
		{"metadata", ResourceMetadata{}, apiclient.AgentMetadata{}},
		{"summary", ResourceSummary{}, apiclient.AgentSummary{}},
		{"labels", ResourceLabels{}, apiclient.ResourceLabels{}},
		{"routeStatus", RouteStatus{}, apiclient.RouteStatus{}},
	} {
		s, c := reflect.TypeOf(tc.server), reflect.TypeOf(tc.client)
		if s != c {
			t.Errorf("%s: server uses %s but client uses %s; they must be one type",
				tc.name, s, c)
		}
		// Both must be the agentapi type, not merely equal to each other.
		if s.PkgPath() != reflect.TypeFor[agentapi.AgentDetail]().PkgPath() {
			t.Errorf("%s: %s is not from agentapi", tc.name, s)
		}
	}
}

// TestSummaryWireShapeMatchesClientType is the list-endpoint counterpart. The
// envelope differs by design (this server has one type where the client has two),
// so only the entries are compared.
func TestSummaryWireShapeMatchesClientType(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	res, err := http.Get(ts.URL + "/api/v1/agents")
	if err != nil {
		t.Fatalf("GET agents: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var got apiclient.AgentListResponse
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("server output does not fit the client's AgentListResponse: %v", err)
	}
	if len(got.Items) == 0 {
		t.Fatal("no items decoded; the round trip proves nothing")
	}
	if got.Items[0].Name == "" || got.Items[0].Status == "" {
		t.Errorf("summary fields did not survive: %+v", got.Items[0])
	}
}

// TestRouteStatusWireShapeMatchesClientType covers the endpoint added alongside
// this refactor, where a stray field would be easiest to introduce unnoticed.
func TestRouteStatusWireShapeMatchesClientType(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	res, err := http.Get(ts.URL + "/api/v1/agents/recorded1/swift-falcon-0001/route-status")
	if err != nil {
		t.Fatalf("GET route-status: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var got apiclient.RouteStatus
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("server output does not fit the client's RouteStatus: %v", err)
	}
	if !got.HasRoute {
		t.Error("hasRoute did not survive the round trip")
	}
}

// TestAgentCardWireShapeMatchesClientType is the guarantee that justifies
// agentCardResponse being a local struct rather than agentapi.AgentCard: the two
// are not the same type, so only a test relates them.
//
// DisallowUnknownFields turns a key the client does not declare into an error. That
// is what makes this meaningful — plain decoding would pass even if this server
// emitted the spec card's nested capabilities block, which the client cannot read.
func TestAgentCardWireShapeMatchesClientType(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, a2aCard, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	res, err := http.Get(ts.URL + "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")
	if err != nil {
		t.Fatalf("GET agent-card: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var got apiclient.AgentCard
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("server output does not fit the client's AgentCard: %v", err)
	}

	// And the decode is not vacuous — the fields the CLI renders arrived.
	if got.Name == "" || got.Version == "" || got.URL == "" {
		t.Errorf("card fields did not survive the round trip: %+v", got)
	}
	if !got.Streaming {
		t.Error("streaming did not survive; the client reads it at the top level")
	}
	if len(got.Skills) != 1 {
		t.Fatalf("skills = %+v, want one entry", got.Skills)
	}
	// The skill keys the client declares, which is the part most likely to drift:
	// this server builds skills as maps, so nothing but this checks the key names.
	s := got.Skills[0]
	if s.ID == "" || s.Name == "" || s.Description == "" {
		t.Errorf("skill fields did not survive: %+v", s)
	}
	if len(s.Tags) != 1 || len(s.Examples) != 1 {
		t.Errorf("skill tags/examples did not survive: %+v", s)
	}
}

// TestLocalInstanceEmitsNullService pins the wire form of the field whose Go type
// changed in the agentapi merge. A nil *ServiceInfo and a nil any both encode as
// null, so the output is unchanged — but that is a property of encoding/json, not
// something the type system enforces, so it is asserted on the raw bytes.
func TestLocalInstanceEmitsNullService(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	res, err := http.Get(ts.URL + "/api/v1/agents/recorded1/swift-falcon-0001")
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	svc, present := raw["service"]
	if !present {
		t.Fatal(`"service" key should be present, as the schema marks it required`)
	}
	if string(svc) != "null" {
		t.Errorf(`"service" = %s, want null for a local instance`, svc)
	}
}

// TestAgentCardNullDescriptionDecodes covers a mismatch DisallowUnknownFields
// cannot see: this server's description is a *string so a card declaring none
// serves null, while the client's field is a plain string. Those are compatible —
// encoding/json leaves a string zero on null — but that is a property of the
// decoder rather than of the two types, so it is asserted rather than assumed.
func TestAgentCardNullDescriptionDecodes(t *testing.T) {
	stubGetter(t, mixedInstances())
	stubCardFetcher(t, `{"name":"Bare","version":"1","url":"http://0.0.0.0:1/"}`, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	res, err := http.Get(ts.URL + "/api/v1/chat/recorded1/swift-falcon-0001/agent-card")
	if err != nil {
		t.Fatalf("GET agent-card: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var got apiclient.AgentCard
	dec := json.NewDecoder(res.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("a null description should decode into the client's string field: %v", err)
	}
	if got.Description != "" {
		t.Errorf("description = %q, want the zero string", got.Description)
	}
	if got.Name == "" {
		t.Error("decode was vacuous; nothing arrived")
	}
}
