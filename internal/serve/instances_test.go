package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rossoctl/rossoctl-cli/internal/instances"
)

// stubLister replaces the instance lister for one test, so the listings can be
// exercised without writing files under a real HOME.
func stubLister(t *testing.T, insts []instances.Instance, err error) {
	t.Helper()
	saved := lister
	lister = func() ([]instances.Instance, error) { return insts, err }
	t.Cleanup(func() { lister = saved })
}

// stubGetter replaces the single-instance reader for one test. It answers from
// insts by namespace and name, so a test supplies records rather than a lookup
// function, and reports a miss the way instances.Get does — wrapping
// fs.ErrNotExist, which is what the handler distinguishes a 404 on.
func stubGetter(t *testing.T, insts []instances.Instance) {
	t.Helper()
	saved := getter
	getter = func(namespace, name string) (*instances.Instance, error) {
		for _, inst := range insts {
			if inst.Namespace == namespace && inst.Name == name {
				return &inst, nil
			}
		}
		return nil, fmt.Errorf("reading instance %s/%s: %w", namespace, name, fs.ErrNotExist)
	}
	t.Cleanup(func() { getter = saved })
}

// stubGetterErr replaces the single-instance reader with one that always fails
// for a reason other than absence.
func stubGetterErr(t *testing.T, err error) {
	t.Helper()
	saved := getter
	getter = func(string, string) (*instances.Instance, error) { return nil, err }
	t.Cleanup(func() { getter = saved })
}

// getResourceList requests path and decodes the listing, failing on a non-200.
func getResourceList(t *testing.T, ts *httptest.Server, path string) ResourceList {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got ResourceList
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return got
}

// names returns the entry names of a listing, for order-sensitive comparison.
func names(list ResourceList) []string {
	out := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, it.Name)
	}
	return out
}

// mixedInstances are two a2a instances and one mcp instance, which is what makes
// the protocol split observable: a handler ignoring the protocol would report all
// three on both endpoints.
//
// The namespaces are deliberately not the server's (see testNamespaces): an
// instance's namespace comes from its record, so fixtures that reused the
// server's list could not tell the two apart.
func mixedInstances() []instances.Instance {
	return []instances.Instance{
		{
			ID:              "11111111-1111-4111-8111-111111111111",
			Name:            "swift-falcon-0001",
			Namespace:       "recorded1",
			InboundProtocol: instances.ProtocolA2A,
			InboundAddr:     "127.0.0.1:8080",
			SessionAddr:     "127.0.0.1:54321",
			CommandLine:     []string{"python", "agent.py"},
			PID:             4242,
		},
		{
			ID:              "22222222-2222-4222-8222-222222222222",
			Name:            "calm-harbor-0002",
			Namespace:       "recorded1",
			InboundProtocol: instances.ProtocolMCP,
			InboundAddr:     "127.0.0.1:8081",
			CommandLine:     []string{"npx", "mcp-server"},
		},
		{
			ID:              "33333333-3333-4333-8333-333333333333",
			Name:            "keen-ridge-0003",
			Namespace:       "recorded2",
			InboundProtocol: instances.ProtocolA2A,
			CommandLine:     []string{"curl", "example.com"},
		},
	}
}

// TestAgentsListsA2AInstances verifies GET /agents reports the a2a instances and
// only those.
func TestAgentsListsA2AInstances(t *testing.T) {
	stubLister(t, mixedInstances(), nil)
	ts := newTestServer(t, "/api/v1")

	got := names(getResourceList(t, ts, "/api/v1/agents"))
	want := []string{"swift-falcon-0001", "keen-ridge-0003"}
	if !slices.Equal(got, want) {
		t.Errorf("agents = %v, want %v", got, want)
	}
}

// TestToolsListsMCPInstances verifies GET /tools reports the mcp instances and
// only those.
func TestToolsListsMCPInstances(t *testing.T) {
	stubLister(t, mixedInstances(), nil)
	ts := newTestServer(t, "/api/v1")

	got := names(getResourceList(t, ts, "/api/v1/tools"))
	want := []string{"calm-harbor-0002"}
	if !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// TestListingEntryFields verifies an entry carries the fields a listing renders
// from: the protocol label, a description naming the hosted command and inbound
// address, and the namespace the instance was recorded in.
func TestListingEntryFields(t *testing.T) {
	stubLister(t, mixedInstances(), nil)
	ts := newTestServer(t, "/api/v1")

	items := getResourceList(t, ts, "/api/v1/agents").Items
	if len(items) == 0 {
		t.Fatal("no entries to inspect")
	}
	got := items[0]

	if want := mixedInstances()[0].Namespace; got.Namespace != want {
		t.Errorf("namespace = %q, want the recorded %q", got.Namespace, want)
	}
	if got.Status == "" {
		t.Error("status is empty; a listing has nothing to show")
	}
	if !slices.Equal(got.Labels.Protocol, []string{string(instances.ProtocolA2A)}) {
		t.Errorf("protocol labels = %v, want [a2a]", got.Labels.Protocol)
	}
	// The description must identify the instance by what it is running.
	for _, want := range []string{"python", "agent.py", "127.0.0.1:8080"} {
		if !strings.Contains(got.Description, want) {
			t.Errorf("description %q does not mention %q", got.Description, want)
		}
	}
}

// TestListingDescriptionIsOneLine verifies an inline shell script renders as a
// single-line label rather than a multi-line block, which would break a listing
// showing one row per instance.
func TestListingDescriptionIsOneLine(t *testing.T) {
	stubLister(t, []instances.Instance{{
		ID:              "55555555-5555-4555-8555-555555555555",
		Name:            "scripted",
		InboundProtocol: instances.ProtocolA2A,
		CommandLine:     []string{"sh", "-c", "\n  echo one\n  echo two\n"},
	}}, nil)
	ts := newTestServer(t, "/api/v1")

	items := getResourceList(t, ts, "/api/v1/agents").Items
	if len(items) != 1 {
		t.Fatalf("got %d entries, want 1", len(items))
	}
	got := items[0].Description

	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("description contains a line break or tab: %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Errorf("description contains a run of spaces: %q", got)
	}
	// The command is still identifiable after collapsing.
	for _, want := range []string{"sh", "echo one", "echo two"} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q lost %q", got, want)
		}
	}
}

// TestListingDescriptionIsBounded verifies a very long command line is elided,
// so one instance cannot dominate a listing payload.
func TestListingDescriptionIsBounded(t *testing.T) {
	long := strings.Repeat("abcdefgh ", 200) // ~1800 bytes
	stubLister(t, []instances.Instance{{
		ID:              "66666666-6666-4666-8666-666666666666",
		Name:            "verbose",
		InboundProtocol: instances.ProtocolA2A,
		CommandLine:     []string{"python", long},
	}}, nil)
	ts := newTestServer(t, "/api/v1")

	got := getResourceList(t, ts, "/api/v1/agents").Items[0].Description
	// The inbound-address suffix is absent here, so the whole description is
	// the elided command.
	if len(got) > maxDescriptionLen+3 {
		t.Errorf("description is %d bytes, want at most %d", len(got), maxDescriptionLen+3)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("a truncated description should be marked elided: %q", got)
	}
	if !strings.HasPrefix(got, "python ") {
		t.Errorf("description %q should still start with the command", got)
	}
}

// TestSummarizeCommandKeepsRunesIntact verifies truncation does not split a
// multi-byte character.
//
// A split would be invisible over HTTP — encoding/json replaces invalid bytes
// with U+FFFD, repairing the damage before a client could see it — so this calls
// summarizeCommand directly.
//
// The offsets matter: the cut is at maxDescriptionLen bytes, so a run of 3-byte
// runes alone would be cut cleanly whenever that limit is a multiple of 3. Each
// case below shifts the run by a different number of leading ASCII bytes, so at
// least one lands mid-rune whatever the limit is.
func TestSummarizeCommandKeepsRunesIntact(t *testing.T) {
	for _, pad := range []string{"", "a", "ab"} {
		got := summarizeCommand([]string{pad + strings.Repeat("あ", 200)})
		if !utf8.ValidString(got) {
			t.Errorf("pad %q: summarizeCommand produced invalid UTF-8: %q", pad, got)
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("pad %q: want an elided result, got %q", pad, got)
		}
	}
}

// TestListingIsReadPerRequest verifies the directory is consulted on each
// request rather than captured at New, since instances start and stop while the
// server runs.
func TestListingIsReadPerRequest(t *testing.T) {
	stubLister(t, nil, nil)
	ts := newTestServer(t, "/api/v1")

	if got := getResourceList(t, ts, "/api/v1/agents"); len(got.Items) != 0 {
		t.Fatalf("agents = %v, want none before any instance exists", names(got))
	}

	// An instance starts after the server was built.
	stubLister(t, mixedInstances(), nil)
	if got := names(getResourceList(t, ts, "/api/v1/agents")); len(got) != 2 {
		t.Errorf("agents = %v, want the two a2a instances started since New", got)
	}
}

// TestListingEmptyEncodesAsArray verifies no instances yields {"items": []}
// rather than {"items": null}, which a client iterating the field would trip on.
func TestListingEmptyEncodesAsArray(t *testing.T) {
	stubLister(t, nil, nil)
	ts := newTestServer(t, "/api/v1")

	for _, path := range []string{"/api/v1/agents", "/api/v1/tools"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		var raw map[string]json.RawMessage
		err = json.NewDecoder(resp.Body).Decode(&raw)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if got := string(raw["items"]); got != "[]" {
			t.Errorf("%s items = %s, want []", path, got)
		}
	}
}

// TestListingSpansNamespaces verifies every namespace directory is read, not just
// one: the two a2a instances are recorded in different namespaces, so a handler
// scanning a single namespace would report only one of them.
func TestListingSpansNamespaces(t *testing.T) {
	stubLister(t, mixedInstances(), nil)
	ts := newTestServer(t, "/api/v1")

	items := getResourceList(t, ts, "/api/v1/agents").Items
	if len(items) != 2 {
		t.Fatalf("agents = %v, want both a2a instances across namespaces", items)
	}
	got := []string{items[0].Namespace, items[1].Namespace}
	if want := []string{"recorded1", "recorded2"}; !slices.Equal(got, want) {
		t.Errorf("namespaces = %v, want each instance in its recorded namespace %v", got, want)
	}
}

// TestListingNamespaceFilter verifies the documented namespace parameter selects
// on the namespace each instance was recorded in, rather than being ignored or
// matched against the server's own list.
func TestListingNamespaceFilter(t *testing.T) {
	stubLister(t, mixedInstances(), nil)
	ts := newTestServer(t, "/api/v1")

	got := names(getResourceList(t, ts, "/api/v1/agents?namespace=recorded1"))
	if want := []string{"swift-falcon-0001"}; !slices.Equal(got, want) {
		t.Errorf("agents in recorded1 = %v, want %v", got, want)
	}
	got = names(getResourceList(t, ts, "/api/v1/agents?namespace=recorded2"))
	if want := []string{"keen-ridge-0003"}; !slices.Equal(got, want) {
		t.Errorf("agents in recorded2 = %v, want %v", got, want)
	}
	if got := getResourceList(t, ts, "/api/v1/agents?namespace=nope"); len(got.Items) != 0 {
		t.Errorf("agents in an unknown namespace = %v, want none", names(got))
	}
	// A namespace the server advertises but no instance was recorded in reports
	// none: the filter matches records, not --namespaces.
	if got := getResourceList(t, ts, "/api/v1/agents?namespace="+testNamespaces[0]); len(got.Items) != 0 {
		t.Errorf("agents in the server's own namespace = %v, want none", names(got))
	}
}

// TestListingUnnamedInstanceFallsBackToID verifies a record with no name still
// lists identifiably rather than as a blank row.
func TestListingUnnamedInstanceFallsBackToID(t *testing.T) {
	stubLister(t, []instances.Instance{{
		ID:              "44444444-4444-4444-8444-444444444444",
		InboundProtocol: instances.ProtocolA2A,
		CommandLine:     []string{"true"},
	}}, nil)
	ts := newTestServer(t, "/api/v1")

	got := names(getResourceList(t, ts, "/api/v1/agents"))
	want := []string{"44444444-4444-4444-8444-444444444444"}
	if !slices.Equal(got, want) {
		t.Errorf("agents = %v, want the id as the name %v", got, want)
	}
}

// TestListingReadFailureIsAnError verifies a directory that cannot be read is
// reported rather than rendered as an empty list — "nothing is running" and
// "cannot tell what is running" are different answers.
func TestListingReadFailureIsAnError(t *testing.T) {
	stubLister(t, nil, errors.New("permission denied"))
	ts := newTestServer(t, "/api/v1")

	for _, path := range []string{"/api/v1/agents", "/api/v1/tools"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		var body map[string]string
		err = json.NewDecoder(resp.Body).Decode(&body)
		code := resp.StatusCode
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if code != http.StatusInternalServerError {
			t.Errorf("%s status = %d, want %d", path, code, http.StatusInternalServerError)
		}
		if !strings.Contains(body["detail"], "permission denied") {
			t.Errorf("%s detail = %q, should report the underlying failure", path, body["detail"])
		}
	}
}

// TestListingIgnoresConfiguredNamespaces verifies a server advertising no
// namespaces still lists instances in the namespaces they were recorded in.
//
// --namespaces says what a UI may offer; an instance's namespace says where it
// actually is. Letting the former decide the latter would hide a running instance
// from a server that happened not to advertise its namespace.
func TestListingIgnoresConfiguredNamespaces(t *testing.T) {
	stubLister(t, mixedInstances(), nil)

	s, err := New("localhost:0", "/api/v1", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	got := getResourceList(t, ts, "/api/v1/agents")
	if len(got.Items) != 2 {
		t.Fatalf("agents = %v, want both a2a instances", names(got))
	}
	if want := "recorded1"; got.Items[0].Namespace != want {
		t.Errorf("namespace = %q, want the recorded %q even though the server serves none",
			got.Items[0].Namespace, want)
	}
}

// getDetail requests path and decodes a detail response, failing on a non-200.
func getDetail(t *testing.T, ts *httptest.Server, path string) ResourceDetail {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var got ResourceDetail
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return got
}

// TestAgentDetailReturnsTheNamedInstance verifies the detail endpoint answers from
// the record the path names, in the namespace the path names.
func TestAgentDetailReturnsTheNamedInstance(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	got := getDetail(t, ts, "/api/v1/agents/recorded1/swift-falcon-0001")

	if got.Metadata.Name != "swift-falcon-0001" {
		t.Errorf("name = %q, want the requested instance", got.Metadata.Name)
	}
	if got.Metadata.Namespace != "recorded1" {
		t.Errorf("namespace = %q, want recorded1", got.Metadata.Namespace)
	}
	if got.ReadyStatus == "" {
		t.Error("readyStatus is empty; a renderer has nothing to show")
	}
	if got.WorkloadType == "" {
		t.Error("workloadType is empty")
	}
	// No cluster service backs a local instance, so this must be null rather than
	// naming an address nothing would answer on.
	if got.Service != nil {
		t.Errorf("service = %v, want null for a local instance", got.Service)
	}
}

// TestAgentDetailIsNamespaceScoped verifies the namespace in the path is not
// ignored: the same name in another namespace is a different record, and here a
// missing one.
func TestAgentDetailIsNamespaceScoped(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	// keen-ridge-0003 exists, but in recorded2.
	resp, err := ts.Client().Get(ts.URL + "/api/v1/agents/recorded1/keen-ridge-0003")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d: the namespace in the path must scope the lookup",
			resp.StatusCode, http.StatusNotFound)
	}

	// And it is found under the namespace it was recorded in.
	if got := getDetail(t, ts, "/api/v1/agents/recorded2/keen-ridge-0003"); got.Metadata.Name != "keen-ridge-0003" {
		t.Errorf("name = %q, want keen-ridge-0003", got.Metadata.Name)
	}
}

// TestAgentDetailCarriesTheRecordsFields verifies the fields only this record
// publishes reach the response — the addresses are the point of asking.
func TestAgentDetailCarriesTheRecordsFields(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	got := getDetail(t, ts, "/api/v1/agents/recorded1/swift-falcon-0001")

	for key, want := range map[string]any{
		"inboundAddr": "127.0.0.1:8080",
		"sessionAddr": "127.0.0.1:54321",
	} {
		if got.Spec[key] != want {
			t.Errorf("spec.%s = %v, want %v", key, got.Spec[key], want)
		}
	}
	// The command line is reported in full, not just as the elided description.
	cmd, ok := got.Spec["command"].([]any)
	if !ok {
		t.Fatalf("spec.command = %v, want an array", got.Spec["command"])
	}
	if len(cmd) != 2 || cmd[0] != "python" || cmd[1] != "agent.py" {
		t.Errorf("spec.command = %v, want [python agent.py]", cmd)
	}
	if desc, _ := got.Spec["description"].(string); !strings.Contains(desc, "agent.py") {
		t.Errorf("spec.description = %q, should name the hosted command", desc)
	}
	// The pid identifies the hosting process, which is what a reader would act on.
	if pid, _ := got.Status["pid"].(float64); int(pid) != 4242 {
		t.Errorf("status.pid = %v, want 4242", got.Status["pid"])
	}
	// The id, not the name, is the uid: it distinguishes two runs of one name.
	if got.Metadata.UID == nil || *got.Metadata.UID != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("uid = %v, want the record's id", got.Metadata.UID)
	}
	// The protocol reaches a label under the key the CLI's renderer reads.
	if got.Metadata.Labels[protocolLabel+string(instances.ProtocolA2A)] == "" {
		t.Errorf("labels = %v, want an a2a protocol label", got.Metadata.Labels)
	}
}

// TestAgentDetailOmitsAbsentAddresses verifies an instance with no inbound
// listener reports no inbound address, rather than an empty string a reader could
// not tell from a blank.
func TestAgentDetailOmitsAbsentAddresses(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	got := getDetail(t, ts, "/api/v1/agents/recorded2/keen-ridge-0003")
	for _, key := range []string{"inboundAddr", "sessionAddr", "adminAddr", "containerName"} {
		if v, ok := got.Spec[key]; ok {
			t.Errorf("spec.%s = %v, want it absent for an instance with no such listener", key, v)
		}
	}
}

// TestAgentDetailReportsReadyCondition verifies the response carries a condition,
// so a renderer showing a conditions table has a row rather than an empty section.
func TestAgentDetailReportsReadyCondition(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	got := getDetail(t, ts, "/api/v1/agents/recorded1/swift-falcon-0001")
	conds, ok := got.Status["conditions"].([]any)
	if !ok || len(conds) == 0 {
		t.Fatalf("status.conditions = %v, want at least one condition", got.Status["conditions"])
	}
	c, ok := conds[0].(map[string]any)
	if !ok {
		t.Fatalf("condition = %v, want an object", conds[0])
	}
	if c["type"] != "Ready" || c["status"] != "True" {
		t.Errorf("condition = %v, want a Ready/True condition", c)
	}
}

// TestAgentDetailMissingIsNotFound verifies an absent instance is a 404 rather
// than a 500 or an empty 200: an instance that shut down since a listing was
// rendered is an ordinary state, and a caller has to tell it from a broken server.
func TestAgentDetailMissingIsNotFound(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	resp, err := ts.Client().Get(ts.URL + "/api/v1/agents/recorded1/gone")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	code := resp.StatusCode
	resp.Body.Close()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", code, http.StatusNotFound)
	}
	if !strings.Contains(body["detail"], "gone") {
		t.Errorf("detail = %q, should name the instance that was not found", body["detail"])
	}
}

// TestAgentDetailReadFailureIsAnError verifies a record that cannot be read is a
// 500 rather than a 404 — "not running" and "cannot tell" are different answers,
// and a refused traversing name must not read as "no such instance", which would
// let a caller probe for which paths are readable.
func TestAgentDetailReadFailureIsAnError(t *testing.T) {
	stubGetterErr(t, errors.New("permission denied"))
	ts := newTestServer(t, "/api/v1")

	resp, err := ts.Client().Get(ts.URL + "/api/v1/agents/recorded1/whatever")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	code := resp.StatusCode
	resp.Body.Close()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", code, http.StatusInternalServerError)
	}
	if !strings.Contains(body["detail"], "permission denied") {
		t.Errorf("detail = %q, should report the underlying failure", body["detail"])
	}
}

// TestAgentDetailRejectsTraversal verifies the real lookup refuses a name that
// would escape the namespace directory, since both path segments arrive from the
// URL.
//
// This drives the production getter rather than a stub: the guard being tested
// lives in instances.Get, and a stub would answer for it.
func TestAgentDetailRejectsTraversal(t *testing.T) {
	ts := newTestServer(t, "/api/v1")

	// %2F keeps the traversal in one path segment, so ServeMux delivers it to the
	// handler as a {name} or {namespace} rather than routing it elsewhere — the
	// escaped slash reaches r.PathValue intact, which is exactly the case the
	// validation exists for.
	//
	// 500 rather than 404: a refused name is not "no such instance", and answering
	// 404 would let a caller distinguish readable paths from unreadable ones by
	// probing.
	for _, path := range []string{
		"/api/v1/agents/recorded1/..%2F..%2Fsecret",
		"/api/v1/agents/..%2F..%2Fsecret/name",
	} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code != http.StatusInternalServerError {
			t.Errorf("GET %s status = %d, want %d", path, code, http.StatusInternalServerError)
		}
	}
}

// TestAgentDetailIsReadPerRequest verifies the record is consulted on each
// request, so an instance that starts after the server appears without a restart.
func TestAgentDetailIsReadPerRequest(t *testing.T) {
	stubGetter(t, nil)
	ts := newTestServer(t, "/api/v1")

	resp, err := ts.Client().Get(ts.URL + "/api/v1/agents/recorded1/swift-falcon-0001")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d before the instance exists", code, http.StatusNotFound)
	}

	// The instance starts after the server was built.
	stubGetter(t, mixedInstances())
	if got := getDetail(t, ts, "/api/v1/agents/recorded1/swift-falcon-0001"); got.Metadata.Name == "" {
		t.Error("the instance started since New is not reported")
	}
}

// TestAgentDetailUnnamedInstanceFallsBackToID verifies a record with no name is
// still reported identifiably rather than with a blank name.
func TestAgentDetailUnnamedInstanceFallsBackToID(t *testing.T) {
	got := detail(instances.Instance{
		ID:              "44444444-4444-4444-8444-444444444444",
		Namespace:       "ns",
		InboundProtocol: instances.ProtocolA2A,
	})
	if got.Metadata.Name != "44444444-4444-4444-8444-444444444444" {
		t.Errorf("name = %q, want the id to stand in", got.Metadata.Name)
	}
}

// TestToolDetailStillUnimplemented verifies GET /tools/{namespace}/{name} was not
// implemented by implementing the agents one — only the agents detail endpoint was
// asked for, and a tools endpoint quietly answering from the same records would
// report mcp instances as agents.
func TestToolDetailStillUnimplemented(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	resp, err := ts.Client().Get(ts.URL + "/api/v1/tools/recorded1/calm-harbor-0002")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

// TestAgentDetailIgnoresMCPInstances verifies an mcp instance is reported absent
// from the agents detail endpoint, matching GET /agents.
//
// Without this the two endpoints would disagree: a name absent from the listing
// would still return a full agent record, describing a tool as an agent.
func TestAgentDetailIgnoresMCPInstances(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	// calm-harbor-0002 exists in recorded1, but its protocol is mcp.
	resp, err := ts.Client().Get(ts.URL + "/api/v1/agents/recorded1/calm-harbor-0002")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d for an mcp instance", resp.StatusCode, http.StatusNotFound)
	}

	// The a2a instance beside it is still found, so this is a protocol check
	// rather than a broken lookup.
	if got := getDetail(t, ts, "/api/v1/agents/recorded1/swift-falcon-0001"); got.Metadata.Name == "" {
		t.Error("the a2a instance in the same namespace should still be found")
	}
}

// routeStatusPath is the route-status endpoint for one instance.
func routeStatusPath(namespace, name string) string {
	return "/api/v1/agents/" + namespace + "/" + name + "/route-status"
}

// TestRouteStatusReportsARoute verifies an existing a2a instance is reported as
// having a route, in the documented envelope.
func TestRouteStatusReportsARoute(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	resp, err := ts.Client().Get(ts.URL + routeStatusPath("recorded1", "swift-falcon-0001"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Decoded into a map so the assertion covers the exact JSON shape the UI
	// reads, not just a struct round-trip.
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{"hasRoute": true}
	if len(got) != len(want) || got["hasRoute"] != true {
		t.Errorf("body = %v, want exactly %v", got, want)
	}
}

// TestRouteStatus404sWhereDetailDoes verifies route-status reports absence in
// exactly the cases the detail endpoint does, which is what it is specified
// against: a missing name, a wrong namespace, and an mcp instance.
func TestRouteStatus404sWhereDetailDoes(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	for _, tc := range []struct{ name, namespace, instance string }{
		{"missing name", "recorded1", "gone"},
		{"wrong namespace", "recorded2", "swift-falcon-0001"},
		{"mcp instance is not an agent", "recorded1", "calm-harbor-0002"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Both endpoints are asked, and the assertion is that they agree —
			// pinning the pair rather than each one's status independently.
			detailResp, err := ts.Client().Get(ts.URL + "/api/v1/agents/" + tc.namespace + "/" + tc.instance)
			if err != nil {
				t.Fatalf("GET detail: %v", err)
			}
			detailResp.Body.Close()

			routeResp, err := ts.Client().Get(ts.URL + routeStatusPath(tc.namespace, tc.instance))
			if err != nil {
				t.Fatalf("GET route-status: %v", err)
			}
			routeResp.Body.Close()

			if detailResp.StatusCode != http.StatusNotFound {
				t.Errorf("detail status = %d, want %d", detailResp.StatusCode, http.StatusNotFound)
			}
			if routeResp.StatusCode != detailResp.StatusCode {
				t.Errorf("route-status = %d, detail = %d; the two must agree",
					routeResp.StatusCode, detailResp.StatusCode)
			}
		})
	}
}

// TestRouteStatusReadFailureIsAnError verifies an unreadable record is a 500
// rather than a 404, matching the detail endpoint: "no route" and "cannot tell"
// are different answers.
func TestRouteStatusReadFailureIsAnError(t *testing.T) {
	stubGetterErr(t, errors.New("permission denied"))
	ts := newTestServer(t, "/api/v1")

	resp, err := ts.Client().Get(ts.URL + routeStatusPath("recorded1", "swift-falcon-0001"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

// TestRouteStatusIsReadPerRequest verifies the record is consulted on every
// request, so an instance that stops while the server runs stops reporting a
// route without a restart.
func TestRouteStatusIsReadPerRequest(t *testing.T) {
	insts := mixedInstances()
	stubGetter(t, insts)
	ts := newTestServer(t, "/api/v1")

	first, err := ts.Client().Get(ts.URL + routeStatusPath("recorded1", "swift-falcon-0001"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want %d", first.StatusCode, http.StatusOK)
	}

	// The instance goes away.
	stubGetter(t, nil)
	second, err := ts.Client().Get(ts.URL + routeStatusPath("recorded1", "swift-falcon-0001"))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Errorf("second status = %d, want %d after the instance stopped",
			second.StatusCode, http.StatusNotFound)
	}
}

// TestToolRouteStatusStillUnimplemented verifies the tools route-status endpoint
// was not implemented by implementing the agents one; only the agents endpoint
// was asked for.
func TestToolRouteStatusStillUnimplemented(t *testing.T) {
	stubGetter(t, mixedInstances())
	ts := newTestServer(t, "/api/v1")

	resp, err := ts.Client().Get(ts.URL + "/api/v1/tools/recorded1/calm-harbor-0002/route-status")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}
