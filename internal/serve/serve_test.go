package serve

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// testNamespaces are the namespaces newTestServer serves, distinct from the
// CLI's defaults so a test cannot pass by accidentally matching those.
var testNamespaces = []string{"nsA", "nsB"}

// newTestServer builds a server mounted at path and returns an httptest server
// driving its handler, so tests exercise real routing without binding a port.
func newTestServer(t *testing.T, path string) *httptest.Server {
	t.Helper()
	s, err := New("localhost:0", path, testNamespaces)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// concretePath replaces the wildcard segments of a route pattern with real
// values so the route can be requested.
func concretePath(p string) string {
	p = strings.ReplaceAll(p, "{namespace}", "ns1")
	return strings.ReplaceAll(p, "{name}", "thing1")
}

// TestAuthConfigReportsDisabled verifies the one implemented operation returns
// 200 with auth disabled.
func TestAuthConfigReportsDisabled(t *testing.T) {
	ts := newTestServer(t, "/api/v1")

	resp, err := http.Get(ts.URL + "/api/v1/auth/config")
	if err != nil {
		t.Fatalf("GET /auth/config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Decode into a map so the assertion covers the exact JSON shape: the
	// nullable Keycloak fields must be omitted, not emitted as null.
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{"enabled": false}
	if len(got) != len(want) {
		t.Fatalf("body = %v, want exactly %v", got, want)
	}
	if got["enabled"] != false {
		t.Errorf("enabled = %v, want false", got["enabled"])
	}
}

// implementedRoutes are the operations with a real implementation, excluded
// from the UNIMPLEMENTED sweep below and covered by their own tests.
//
// Keyed by "METHOD PATH" rather than by Route: a Route carries a handler
// function, which makes it incomparable and so unusable as a map key.
var implementedRoutes = map[string]bool{
	http.MethodGet + " /auth/config":                            true,
	http.MethodGet + " /namespaces":                             true,
	http.MethodGet + " /agents":                                 true,
	http.MethodGet + " /tools":                                  true,
	http.MethodGet + " /agents/{namespace}/{name}":              true,
	http.MethodGet + " /agents/{namespace}/{name}/route-status": true,
	http.MethodGet + " /chat/{namespace}/{name}/agent-card":     true,
}

// routeKey is the implementedRoutes key for r.
func routeKey(r Route) string { return r.Method + " " + r.Path }

// TestAllOtherRoutesUnimplemented verifies every documented operation is
// routed, and that all but the implemented ones answer 500 UNIMPLEMENTED. A 404
// here would mean a route is missing from the table.
func TestAllOtherRoutesUnimplemented(t *testing.T) {
	ts := newTestServer(t, "/api/v1")

	for _, r := range APIRoutes() {
		if implementedRoutes[routeKey(r)] {
			continue
		}
		t.Run(r.Method+" "+r.Path, func(t *testing.T) {
			req, err := http.NewRequest(r.Method, ts.URL+"/api/v1"+concretePath(r.Path), nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
			}
			var body map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["detail"] != unimplementedMessage {
				t.Errorf("detail = %q, want %q", body["detail"], unimplementedMessage)
			}
		})
	}
}

// TestNamespacesReportsConfiguredList verifies GET /namespaces returns the
// namespaces New was given, in order, inside the documented envelope.
func TestNamespacesReportsConfiguredList(t *testing.T) {
	ts := newTestServer(t, "/api/v1")

	resp, err := http.Get(ts.URL + "/api/v1/namespaces")
	if err != nil {
		t.Fatalf("GET /namespaces: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var got NamespaceList
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Equal(got.Namespaces, testNamespaces) {
		t.Errorf("namespaces = %v, want %v", got.Namespaces, testNamespaces)
	}
}

// TestNamespacesIgnoresEnabledOnly verifies the documented enabled_only query
// parameter does not change the reported list — neither value hides it.
func TestNamespacesIgnoresEnabledOnly(t *testing.T) {
	ts := newTestServer(t, "/api/v1")

	for _, q := range []string{"", "?enabled_only=true", "?enabled_only=false"} {
		resp, err := http.Get(ts.URL + "/api/v1/namespaces" + q)
		if err != nil {
			t.Fatalf("GET /namespaces%s: %v", q, err)
		}
		var got NamespaceList
		err = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !slices.Equal(got.Namespaces, testNamespaces) {
			t.Errorf("GET /namespaces%s = %v, want %v", q, got.Namespaces, testNamespaces)
		}
	}
}

// TestNamespacesEmptyIsArrayNotNull verifies a server built with no namespaces
// serves [] rather than null, which the required-field schema demands and which
// a JSON client can iterate without a nil check.
func TestNamespacesEmptyIsArrayNotNull(t *testing.T) {
	s, err := New("localhost:0", "/api/v1", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/namespaces")
	if err != nil {
		t.Fatalf("GET /namespaces: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := strings.TrimSpace(string(body)), `{"namespaces":[]}`; got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// TestNamespacesAreCopied verifies the server does not alias the caller's slice,
// so mutating it after New cannot change what is served.
func TestNamespacesAreCopied(t *testing.T) {
	ns := []string{"team1", "team2"}
	s, err := New("localhost:0", "/api/v1", ns)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ns[0] = "mutated"
	if got := s.Namespaces(); got[0] != "team1" {
		t.Errorf("namespaces[0] = %q, want %q — New aliased the caller's slice", got[0], "team1")
	}

	// Namespaces() must hand out a copy too.
	got := s.Namespaces()
	got[0] = "mutated"
	if s.Namespaces()[0] != "team1" {
		t.Error("mutating the Namespaces result changed the server's list")
	}
}

// TestSplitNamespaces covers the --namespaces parsing: trimming, dropped empty
// entries, and preserved order.
func TestSplitNamespaces(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"team1,team2", []string{"team1", "team2"}},
		{"team1, team2", []string{"team1", "team2"}},
		{"  team1 , team2  ", []string{"team1", "team2"}},
		{"team1,,team2,", []string{"team1", "team2"}},
		{"solo", []string{"solo"}},
		// Order is preserved rather than sorted.
		{"zeta,alpha", []string{"zeta", "alpha"}},
		{"", []string{}},
		{",", []string{}},
		{"  ", []string{}},
	} {
		got := SplitNamespaces(tc.in)
		if !slices.Equal(got, tc.want) {
			t.Errorf("SplitNamespaces(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if got == nil {
			t.Errorf("SplitNamespaces(%q) returned nil, want an empty slice", tc.in)
		}
	}
}

// TestListenReportsConcreteAddress verifies Listen's address is concrete: asking
// for port 0 reports the kernel-assigned port, not ":0". This is the property
// that makes bind-then-report worth doing.
func TestListenReportsConcreteAddress(t *testing.T) {
	s, err := New("127.0.0.1:0", "/api/v1", testNamespaces)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := s.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	if s.Addr() != "127.0.0.1:0" {
		t.Errorf("Addr() = %q, want the requested address to be unchanged", s.Addr())
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", ln.Addr(), err)
	}
	if port == "0" || port == "" {
		t.Errorf("listener port = %q, want a kernel-assigned port", port)
	}
}

// TestListenThenServe verifies the documented pairing serves requests on the
// listener Listen returned.
func TestListenThenServe(t *testing.T) {
	s, err := New("127.0.0.1:0", "/api/v1", testNamespaces)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := s.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve(ln) }()
	defer ln.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/api/v1/namespaces")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestListenPortInUse verifies a taken port surfaces as an error from Listen
// rather than being silently shared. Binding is the availability check, so this
// covers the case a pre-bind probe would have raced on.
func TestListenPortInUse(t *testing.T) {
	first, err := New("127.0.0.1:0", "/api/v1", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln, err := first.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	// Ask for the exact address already bound.
	second, err := New(ln.Addr().String(), "/api/v1", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ln2, err := second.Listen()
	if err == nil {
		ln2.Close()
		t.Fatal("Listen on a bound port should fail")
	}
	if !strings.Contains(err.Error(), ln.Addr().String()) {
		t.Errorf("error should name the address: %v", err)
	}
}

// TestRouteTableMatchesOpenAPI guards the operation count against accidental
// edits to the route table. The backend's OpenAPI document lists 43 operations
// under /api/v1 plus /health and /ready at the root.
func TestRouteTableMatchesOpenAPI(t *testing.T) {
	if got, want := len(APIRoutes()), 43; got != want {
		t.Errorf("API route count = %d, want %d", got, want)
	}
	if got, want := len(HealthRoutes()), 2; got != want {
		t.Errorf("health route count = %d, want %d", got, want)
	}
}

// TestAPIRoutesIsACopy verifies the accessors hand out copies, so a caller
// mutating the result cannot corrupt the routing table.
// TestRouteHandlersDeclaredInTable verifies the table is the source of dispatch:
// every route names a handler, and exactly the implemented ones name something
// other than the placeholder.
//
// This is what keeps the table honest now that New does no dispatching of its
// own — a route whose Handler was left nil would silently serve UNIMPLEMENTED.
func TestRouteHandlersDeclaredInTable(t *testing.T) {
	placeholder := reflect.ValueOf(unimplemented).Pointer()

	for _, r := range append(APIRoutes(), HealthRoutes()...) {
		key := routeKey(r)
		if r.Handler == nil {
			t.Errorf("%s declares no handler", key)
			continue
		}
		isReal := reflect.ValueOf(r.Handler).Pointer() != placeholder
		if want := implementedRoutes[key]; isReal != want {
			t.Errorf("%s: real handler = %v, want %v", key, isReal, want)
		}
	}
}

// TestNilHandlerFallsBackToUnimplemented verifies a route added to the table
// without a handler answers UNIMPLEMENTED rather than panicking at startup.
func TestNilHandlerFallsBackToUnimplemented(t *testing.T) {
	h := Route{Method: http.MethodGet, Path: "/new"}.handler(opts{})
	if h == nil {
		t.Fatal("handler() returned nil for a route with no Handler")
	}

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/new", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// A Route holds a handler function, so it is incomparable: the assertion is on
// the method and path, which is what identifies an operation.
func TestAPIRoutesIsACopy(t *testing.T) {
	got := APIRoutes()
	first := routeKey(got[0])
	got[0] = Route{Method: http.MethodPatch, Path: "/mutated"}
	if routeKey(APIRoutes()[0]) != first {
		t.Error("mutating the APIRoutes result changed the underlying table")
	}
}

// TestHealthServedAtRootAndPrefix verifies the root-level health probes are
// reachable both at the root and under the mount path.
func TestHealthServedAtRootAndPrefix(t *testing.T) {
	ts := newTestServer(t, "/api/v1")

	for _, p := range []string{"/health", "/ready", "/api/v1/health", "/api/v1/ready"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		// Routed but unimplemented; 404 would mean it is not registered.
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("GET %s status = %d, want %d", p, resp.StatusCode, http.StatusInternalServerError)
		}
	}
}

// TestRootMountAvoidsHealthCollision verifies that mounting at the root does not
// panic from registering /health twice, and still serves it.
func TestRootMountAvoidsHealthCollision(t *testing.T) {
	ts := newTestServer(t, "")

	for _, p := range []string{"/health", "/auth/config"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("GET %s = 404, want the route to be registered at the root", p)
		}
	}
}

// TestUnknownPathIs404 verifies undocumented paths are not swept into the
// UNIMPLEMENTED placeholder — only real operations are routed.
func TestUnknownPathIs404(t *testing.T) {
	ts := newTestServer(t, "/api/v1")

	resp, err := http.Get(ts.URL + "/api/v1/nonesuch")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestWrongMethodIsNotFoundOrNotAllowed verifies a known path with an
// undocumented method is rejected rather than answered. ServeMux reports 405
// for a path registered under other methods.
func TestWrongMethodIsNotAllowed(t *testing.T) {
	ts := newTestServer(t, "/api/v1")

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/namespaces", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// TestNewNormalizesPath verifies the mount path is normalized so equivalent
// spellings mount identically.
func TestNewNormalizesPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/api/v1", "/api/v1"},
		{"api/v1", "/api/v1"},
		{"/api/v1/", "/api/v1"},
		{"", ""},
		{"/", ""},
	} {
		s, err := New("localhost:0", tc.in, nil)
		if err != nil {
			t.Fatalf("New(%q): %v", tc.in, err)
		}
		if s.Path() != tc.want {
			t.Errorf("New(%q).Path() = %q, want %q", tc.in, s.Path(), tc.want)
		}
	}
}

// TestNewRejectsBadAddress verifies a listen address missing a port is caught at
// construction rather than at bind time.
func TestNewRejectsBadAddress(t *testing.T) {
	if _, err := New("localhost", "/api/v1", nil); err == nil {
		t.Error("New should reject an address with no port")
	}
}

// TestSplitAddress covers the --address parsing: host:port with and without a
// trailing path, and the rejection of a scheme.
func TestSplitAddress(t *testing.T) {
	for _, tc := range []struct {
		in      string
		addr    string
		path    string
		wantErr bool
	}{
		{in: "localhost:9093/api/v1", addr: "localhost:9093", path: "/api/v1"},
		{in: "localhost:9093", addr: "localhost:9093", path: ""},
		{in: ":9093/api", addr: ":9093", path: "/api"},
		{in: "127.0.0.1:9093/", addr: "127.0.0.1:9093", path: "/"},
		{in: "http://localhost:9093/api/v1", wantErr: true},
		{in: "localhost/api/v1", wantErr: true},
		{in: "localhost", wantErr: true},
	} {
		addr, path, err := SplitAddress(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("SplitAddress(%q) should error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitAddress(%q): %v", tc.in, err)
			continue
		}
		if addr != tc.addr || path != tc.path {
			t.Errorf("SplitAddress(%q) = (%q, %q), want (%q, %q)", tc.in, addr, path, tc.addr, tc.path)
		}
	}
}

// TestAddrRoundTrips verifies the listen address is reported back unchanged.
func TestAddrRoundTrips(t *testing.T) {
	s, err := New("localhost:9093", "/api/v1", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Addr() != "localhost:9093" {
		t.Errorf("Addr() = %q, want %q", s.Addr(), "localhost:9093")
	}
}
