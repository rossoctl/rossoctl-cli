// Package serve hosts a local implementation of the rossoctl backend API.
//
// The API surface mirrors the OpenAPI document the real backend publishes at
// /api/openapi.json: every operation in that document is routed here, so a UI
// pointed at this server sees the same set of endpoints rather than a wall of
// 404s. Most are placeholders that answer 500 UNIMPLEMENTED. Six are real:
//
//   - GET /auth/config reports authentication as disabled, so a UI can finish
//     initializing without a Keycloak realm behind it.
//   - GET /namespaces reports the namespaces passed to New.
//   - GET /agents reports the AuthBridge instances running on this host whose
//     inbound protocol is a2a.
//   - GET /tools reports the same for instances whose inbound protocol is mcp.
//   - GET /agents/{namespace}/{name} reports one instance in detail.
//   - GET /agents/{namespace}/{name}/route-status reports that an existing
//     instance has a route, since one reached at its inbound address always
//     does; it 404s exactly when the detail endpoint does.
//
// The instance endpoints read the instances directory on every request rather
// than once at startup: instances are started and stopped by `authbridge exec`
// while this server runs, so a list captured at New would go stale immediately.
//
// Each instance is reported in the namespace it was recorded in, which is the
// directory its record lives in — not in one of the namespaces passed to New.
// The two lists are independent: --namespaces says what a UI may offer, while an
// instance's namespace says where it actually is, and an instance recorded in a
// namespace the server does not advertise is still reported rather than hidden.
//
// Each entry in the route table names its own handler, so the table is the whole
// answer to "what serves this path" — there is no dispatch logic elsewhere to
// keep in step with it.
//
// Like the other internal packages this one is free of Cobra: New takes a
// listen address, a mount path, and the namespaces to serve, and the caller
// decides when to serve. That keeps it testable over an httptest server with no
// CLI involved.
package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/rossoctl/rossoctl-cli/internal/instances"
)

// unimplementedMessage is the body detail returned by every placeholder route.
// FastAPI-style backends report errors as {"detail": ...}, so failures from
// this server deserialize the same way as failures from the real one.
const unimplementedMessage = "UNIMPLEMENTED"

// Route is one operation in the served API: an HTTP method, a path relative to
// the server's mount path, and the handler that answers it. Paths use Go 1.22+
// ServeMux wildcard syntax, so the OpenAPI templates {namespace}/{name} appear
// here as {namespace}/{name}.
//
// Handler is a factory rather than an http.Handler because some routes need
// values fixed at New time (the served namespaces). Declaring it per route keeps
// the association between an operation and its implementation in one place: the
// table below is the whole answer to "what serves this path".
//
// A nil Handler means the route is a placeholder answering 500 UNIMPLEMENTED,
// which is most of them — see unimplemented.
type Route struct {
	Method  string
	Path    string
	Handler func(opts) http.HandlerFunc
}

// opts is the construction-time state a route's handler may need. It is passed
// to every Handler factory, which takes what it wants and ignores the rest.
type opts struct {
	// namespaces are the namespaces this server reports, in order.
	namespaces []string
}

// unimplemented is the Handler for a placeholder route. It exists so the table
// can name what a route does rather than leaving a blank column, which would
// read as an oversight rather than a decision.
func unimplemented(opts) http.HandlerFunc { return unimplementedHandler }

// handler builds the route's handler, falling back to the placeholder when the
// route declares none. The fallback means a route appended to the table without
// a handler answers UNIMPLEMENTED — the same as an explicit placeholder — rather
// than panicking on a nil call at startup.
func (r Route) handler(o opts) http.HandlerFunc {
	if r.Handler == nil {
		return unimplementedHandler
	}
	return r.Handler(o)
}

// apiRoutes are the operations under the mount path, transcribed from the
// backend's OpenAPI document with its /api/v1 prefix stripped — that prefix is
// supplied by the mount path instead, so a server mounted at /api/v1 reproduces
// the documented URLs exactly.
var apiRoutes = []Route{
	// agents
	{http.MethodGet, "/agents", agentsRoute},
	{http.MethodPost, "/agents", unimplemented},
	{http.MethodGet, "/agents/build-strategies", unimplemented},
	{http.MethodPost, "/agents/fetch-env-url", unimplemented},
	{http.MethodGet, "/agents/migration/migratable", unimplemented},
	{http.MethodPost, "/agents/migration/migrate-all", unimplemented},
	{http.MethodPost, "/agents/parse-env", unimplemented},
	{http.MethodGet, "/agents/shipwright-builds", unimplemented},
	{http.MethodDelete, "/agents/{namespace}/{name}", unimplemented},
	{http.MethodGet, "/agents/{namespace}/{name}", agentDetailRoute},
	{http.MethodPost, "/agents/{namespace}/{name}/finalize-shipwright-build", unimplemented},
	{http.MethodGet, "/agents/{namespace}/{name}/identity-config", unimplemented},
	{http.MethodGet, "/agents/{namespace}/{name}/identity-status", unimplemented},
	{http.MethodPost, "/agents/{namespace}/{name}/migrate", unimplemented},
	{http.MethodGet, "/agents/{namespace}/{name}/route-status", agentRouteStatusRoute},
	{http.MethodGet, "/agents/{namespace}/{name}/shipwright-build", unimplemented},
	{http.MethodGet, "/agents/{namespace}/{name}/shipwright-build-info", unimplemented},
	{http.MethodGet, "/agents/{namespace}/{name}/shipwright-buildrun", unimplemented},
	{http.MethodPost, "/agents/{namespace}/{name}/shipwright-buildrun", unimplemented},

	// auth
	{http.MethodGet, "/auth/config", authConfigRoute},
	{http.MethodGet, "/auth/me", unimplemented},
	{http.MethodGet, "/auth/status", unimplemented},
	{http.MethodGet, "/auth/userinfo", unimplemented},

	// chat
	{http.MethodGet, "/chat/{namespace}/{name}/agent-card", unimplemented},
	{http.MethodPost, "/chat/{namespace}/{name}/send", unimplemented},
	{http.MethodPost, "/chat/{namespace}/{name}/stream", unimplemented},

	// config
	{http.MethodGet, "/config/dashboards", unimplemented},
	{http.MethodGet, "/config/features", unimplemented},
	{http.MethodGet, "/config/mcp-gateway-status", unimplemented},
	{http.MethodGet, "/config/platform-status", unimplemented},

	// namespaces
	{http.MethodGet, "/namespaces", namespacesRoute},

	// shipwright
	{http.MethodGet, "/shipwright/builds", unimplemented},

	// tools
	{http.MethodGet, "/tools", toolsRoute},
	{http.MethodPost, "/tools", unimplemented},
	{http.MethodGet, "/tools/shipwright-builds", unimplemented},
	{http.MethodDelete, "/tools/{namespace}/{name}", unimplemented},
	{http.MethodGet, "/tools/{namespace}/{name}", unimplemented},
	{http.MethodPost, "/tools/{namespace}/{name}/connect", unimplemented},
	{http.MethodPost, "/tools/{namespace}/{name}/finalize-shipwright-build", unimplemented},
	{http.MethodPost, "/tools/{namespace}/{name}/invoke", unimplemented},
	{http.MethodGet, "/tools/{namespace}/{name}/route-status", unimplemented},
	{http.MethodGet, "/tools/{namespace}/{name}/shipwright-build-info", unimplemented},
	{http.MethodPost, "/tools/{namespace}/{name}/shipwright-buildrun", unimplemented},
}

// healthRoutes are the operations the OpenAPI document places at the site root
// rather than under /api/v1. They are registered both at the root and under the
// mount path, so a probe finds them wherever it looks.
var healthRoutes = []Route{
	{http.MethodGet, "/health", unimplemented},
	{http.MethodGet, "/ready", unimplemented},
}

// APIRoutes returns the operations served under the mount path. The result is a
// copy, so callers cannot disturb the routing table.
func APIRoutes() []Route { return append([]Route(nil), apiRoutes...) }

// HealthRoutes returns the root-level health operations. The result is a copy.
func HealthRoutes() []Route { return append([]Route(nil), healthRoutes...) }

// AuthConfig is the GET /auth/config response, mirroring the backend's
// AuthConfigResponse schema. Only Enabled is required there; the Keycloak
// fields are nullable and omitted while auth is disabled.
type AuthConfig struct {
	Enabled     bool    `json:"enabled"`
	KeycloakURL *string `json:"keycloak_url,omitempty"`
	Realm       *string `json:"realm,omitempty"`
	ClientID    *string `json:"client_id,omitempty"`
	RedirectURI *string `json:"redirect_uri,omitempty"`
}

// NamespaceList is the GET /namespaces response, mirroring the backend's
// NamespaceListResponse schema. The slice is always emitted, as an empty array
// rather than null, because the schema marks it required.
type NamespaceList struct {
	Namespaces []string `json:"namespaces"`
}

// ResourceLabels is the labels block of a summary entry, mirroring the
// backend's ResourceLabels schema. Framework and Type are nullable there, so
// they are pointers and encode as null when unknown — which they are for a
// locally hosted instance, since nothing declares them.
type ResourceLabels struct {
	Protocol  []string `json:"protocol"`
	Framework *string  `json:"framework"`
	Type      *string  `json:"type"`
}

// ResourceSummary is one entry in a GET /agents or GET /tools response,
// mirroring the backend's AgentSummary and ToolSummary schemas. Those two have
// the same shape, so one type serves both.
//
// WorkloadType and CreatedAt are nullable in the schema and stay null here: an
// instance is a local process, not a cluster workload, and its record carries no
// start time.
type ResourceSummary struct {
	Name         string         `json:"name"`
	Namespace    string         `json:"namespace"`
	Description  string         `json:"description"`
	Status       string         `json:"status"`
	Labels       ResourceLabels `json:"labels"`
	WorkloadType *string        `json:"workloadType"`
	CreatedAt    *string        `json:"createdAt"`
}

// ResourceList is a GET /agents or GET /tools response, mirroring the backend's
// AgentListResponse and ToolListResponse schemas. The slice is always emitted,
// as an empty array rather than null, because the schema marks it required.
type ResourceList struct {
	Items []ResourceSummary `json:"items"`
}

// ResourceMetadata is the metadata block of a detail response, mirroring the
// backend's metadata schema. CreationTimestamp is nullable and stays null: an
// instance record carries no start time.
type ResourceMetadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp *string           `json:"creationTimestamp"`
	UID               *string           `json:"uid"`
}

// ResourceDetail is the GET /agents/{namespace}/{name} response, mirroring the
// backend's AgentDetailResponse schema.
//
// Spec and Status are open maps in that schema — the backend fills them from a
// Kubernetes custom resource, which has no fixed shape here — so the instance's
// own fields go into them under names the CLI's renderer already reads.
//
// WorkloadType and Service describe cluster deployment. An instance is a local
// process, so the workload type says so and Service is null rather than naming a
// ClusterIP nothing would answer on.
type ResourceDetail struct {
	Metadata     ResourceMetadata `json:"metadata"`
	Spec         map[string]any   `json:"spec"`
	Status       map[string]any   `json:"status"`
	WorkloadType string           `json:"workloadType"`
	ReadyStatus  string           `json:"readyStatus"`
	Service      any              `json:"service"`
}

// Server is a configured API server. Build one with New, then call
// ListenAndServe to run it or Handler to drive it in-process.
type Server struct {
	addr       string
	path       string
	namespaces []string
	handler    http.Handler
}

// lister and getter read the AuthBridge instances running on this host. They are
// variables so a test can supply records without writing files under a real HOME;
// production always uses the instances package.
var (
	lister = instances.List
	getter = instances.Get
)

// New builds a server that listens on addr, mounts the API at path, and reports
// namespaces from GET /namespaces.
//
// addr and path are typically taken from a single "host:port/path" string; see
// SplitAddress. The path is normalized to a leading-slash, no-trailing-slash
// prefix, so "api/v1", "/api/v1", and "/api/v1/" mount identically. An empty
// path mounts the API at the root.
//
// namespaces is served as-is, in order; a nil or empty slice yields an empty
// JSON array rather than null. New copies it, so later changes by the caller do
// not affect the running server.
//
// New returns an error only if addr is unusable as a listen address; it does
// not bind anything, so a New that succeeds can still fail in ListenAndServe
// if the port is already taken.
func New(addr, path string, namespaces []string) (*Server, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", addr, err)
	}

	prefix := normalizePath(path)
	mux := http.NewServeMux()

	// Copy so the caller cannot mutate what the handler serves, and so the
	// JSON encodes as [] rather than null when nothing was supplied.
	ns := make([]string, 0, len(namespaces))
	ns = append(ns, namespaces...)

	// Every documented API operation, each served by the handler its table entry
	// names. Registering with an explicit method means a wrong method on a known
	// path gets ServeMux's 405 rather than an UNIMPLEMENTED 500.
	o := opts{namespaces: ns}
	for _, r := range apiRoutes {
		mux.HandleFunc(fmt.Sprintf("%s %s%s", r.Method, prefix, r.Path), r.handler(o))
	}

	// Health probes at the root, and again under the mount path when that is
	// not itself the root (where they would collide).
	for _, r := range healthRoutes {
		h := r.handler(o)
		mux.HandleFunc(fmt.Sprintf("%s %s", r.Method, r.Path), h)
		if prefix != "" {
			mux.HandleFunc(fmt.Sprintf("%s %s%s", r.Method, prefix, r.Path), h)
		}
	}

	return &Server{addr: addr, path: prefix, namespaces: ns, handler: mux}, nil
}

// Addr returns the listen address the server was built with. This is the
// requested address, which may name a hostname rather than the concrete address
// finally bound — see Listen for that.
func (s *Server) Addr() string { return s.addr }

// Namespaces returns the namespaces served by GET /namespaces. The result is a
// copy.
func (s *Server) Namespaces() []string { return append([]string(nil), s.namespaces...) }

// Path returns the normalized mount path: a leading-slash, no-trailing-slash
// prefix, or "" when the API is mounted at the root.
func (s *Server) Path() string { return s.path }

// Handler returns the server's routes as an http.Handler, for tests and for
// callers that want to wrap or embed them.
func (s *Server) Handler() http.Handler { return s.handler }

// Listen binds the server's listen address and returns the listener without
// serving on it.
//
// Binding is itself the port-availability check: it is atomic and enforced by
// the kernel, so there is no window in which another process can take the port
// between a check and the bind. A port already in use surfaces here as an
// EADDRINUSE-wrapping error.
//
// Callers should report the returned listener's Addr rather than the address
// they asked for. The two can differ in ways that matter: a hostname resolving
// to several addresses is bound at only the first (so "localhost:9097" binds
// 127.0.0.1 but not [::1]), and port 0 is assigned a real port by the kernel.
// Reporting the requested address instead can advertise a URL that reaches a
// different process, or no process at all.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", s.addr, err)
	}
	return ln, nil
}

// Serve serves the API on ln until the server fails, and always returns a
// non-nil error. It takes ownership of ln and closes it on return.
//
// Pair it with Listen when the concrete bound address must be reported before
// serving begins:
//
//	ln, err := srv.Listen()
//	if err != nil { return err }
//	fmt.Printf("serving on http://%s%s\n", ln.Addr(), srv.Path())
//	return srv.Serve(ln)
func (s *Server) Serve(ln net.Listener) error {
	return (&http.Server{Handler: s.handler}).Serve(ln)
}

// ListenAndServe binds the listen address and serves until the server fails.
// It always returns a non-nil error.
//
// It reports nothing, so the bound address stays invisible to the user. Prefer
// Listen plus Serve where that address should be printed.
func (s *Server) ListenAndServe() error {
	ln, err := s.Listen()
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// SplitAddress splits a "host:port/path" string into its listen address and
// mount path, the form the --address flag accepts. The path is everything from
// the first slash after the port, and is empty when none is given:
//
//	"localhost:9093/api/v1" -> "localhost:9093", "/api/v1"
//	"localhost:9093"        -> "localhost:9093", ""
//	":9093/api"             -> ":9093",          "/api"
//
// A scheme, if present, is rejected rather than quietly ignored, since it would
// otherwise be parsed as part of the host.
func SplitAddress(address string) (addr, path string, err error) {
	if strings.Contains(address, "://") {
		return "", "", fmt.Errorf("address %q must not include a scheme", address)
	}

	addr, path = address, ""
	if i := strings.Index(address, "/"); i >= 0 {
		addr, path = address[:i], address[i:]
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", "", fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	return addr, path, nil
}

// SplitNamespaces splits a comma-separated namespace list into a slice, the
// form the --namespaces flag accepts. Surrounding whitespace is trimmed and
// empty entries are dropped, so "team1, team2", "team1,team2", and
// "team1,,team2," all yield the same two namespaces. Order is preserved.
//
// An empty or all-empty string yields an empty slice, which serves as an empty
// JSON array rather than an error: a server advertising no namespaces is a
// legitimate thing to ask for.
func SplitNamespaces(list string) []string {
	out := make([]string, 0)
	for part := range strings.SplitSeq(list, ",") {
		if ns := strings.TrimSpace(part); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// normalizePath turns a mount path into a leading-slash, no-trailing-slash
// prefix. The root ("", "/") normalizes to "" so it concatenates cleanly with
// route paths, which each start with a slash of their own.
func normalizePath(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	return "/" + path
}

// authConfigRoute serves GET /auth/config, reporting auth as disabled: a UI asks
// for it before anything else to decide whether to start a login flow, and
// answering "disabled" lets it proceed straight to the API.
func authConfigRoute(opts) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, AuthConfig{Enabled: false})
	}
}

// namespacesRoute serves GET /namespaces from the list New was given.
//
// The documented enabled_only query parameter is accepted and ignored: these
// namespaces carry no enabled/disabled state, so both values report the same
// list rather than one of them reporting nothing.
func namespacesRoute(o opts) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, NamespaceList{Namespaces: o.namespaces})
	}
}

// agentsRoute serves GET /agents: the running instances that front an a2a
// endpoint.
func agentsRoute(opts) http.HandlerFunc {
	return instanceListHandler(instances.ProtocolA2A)
}

// toolsRoute serves GET /tools: the running instances that front an mcp
// endpoint.
func toolsRoute(opts) http.HandlerFunc {
	return instanceListHandler(instances.ProtocolMCP)
}

// instanceListHandler serves GET /agents (proto a2a) or GET /tools (proto mcp)
// from the instances directory, one entry per running instance whose inbound
// protocol matches.
//
// Every namespace directory is read on every request, so an instance started
// after this server appears without a restart, and one that has shut down stops
// appearing. A read failure is reported as a 500 rather than an empty list: "no
// instances running" and "cannot tell what is running" are different answers,
// and a UI showing an empty list for the second would be showing a falsehood.
//
// The documented namespace query parameter filters on the namespace each
// instance was actually recorded in, not on the namespaces passed to New: an
// instance's namespace is a property of the record, so a filter has something
// real to match against and the server's own list plays no part.
func instanceListHandler(proto instances.Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		insts, err := lister()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError,
				map[string]string{"detail": fmt.Sprintf("listing local instances: %v", err)})
			return
		}

		// Always a non-nil slice, so an empty result encodes as [] not null.
		out := ResourceList{Items: make([]ResourceSummary, 0, len(insts))}

		want := r.URL.Query().Get("namespace")
		for _, inst := range insts {
			if inst.InboundProtocol != proto {
				continue
			}
			if want != "" && inst.Namespace != want {
				continue
			}
			out.Items = append(out.Items, summarize(inst))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// agentDetailRoute serves GET /agents/{namespace}/{name} from the one record
// that names it.
//
// A record that is not there is a 404 rather than a 500: an instance that has
// shut down since a listing was rendered is an ordinary state, and the caller
// needs to tell it apart from a directory it cannot read. instances.Get
// distinguishes the two, so this switches on fs.ErrNotExist rather than treating
// every failure alike.
//
// A name or namespace that could escape the instances directory is refused by
// instances.Get, which matters here because both arrive from the URL path. That
// refusal is not a "not exist", so it surfaces as a 500 rather than inviting a
// caller to probe for which paths are readable.
//
// An instance whose inbound protocol is not a2a is reported as absent, matching
// GET /agents: the two endpoints answer about the same set of things, and a
// detail endpoint that described an mcp instance as an agent would contradict the
// listing it was reached from.
func agentDetailRoute(opts) http.HandlerFunc {
	return instanceDetailHandler(instances.ProtocolA2A)
}

// instanceDetailHandler serves the detail endpoint for one protocol; see
// agentDetailRoute.
func instanceDetailHandler(proto instances.Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, ok := lookupInstance(w, r, proto)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, detail(*inst))
	}
}

// lookupInstance resolves the {namespace}/{name} pair of a detail-style request
// to one instance of the given protocol, writing the error response and
// reporting false when it cannot.
//
// Shared by the detail and route-status endpoints so the two cannot disagree
// about what exists: route-status is specified to 404 exactly when the detail
// endpoint would, and a second copy of this lookup would only stay in agreement
// by coincidence.
func lookupInstance(w http.ResponseWriter, r *http.Request, proto instances.Protocol) (*instances.Instance, bool) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")

	notFound := func() {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"detail": fmt.Sprintf("no %s instance %q in namespace %q is running", proto, name, namespace),
		})
	}

	inst, err := getter(namespace, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			notFound()
			return nil, false
		}
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"detail": fmt.Sprintf("reading local instance: %v", err)})
		return nil, false
	}
	if inst.InboundProtocol != proto {
		notFound()
		return nil, false
	}
	return inst, true
}

// RouteStatus is the GET /agents/{namespace}/{name}/route-status response,
// reporting whether an HTTPRoute exposes the instance.
type RouteStatus struct {
	HasRoute bool `json:"hasRoute"`
}

// agentRouteStatusRoute serves GET /agents/{namespace}/{name}/route-status.
//
// HasRoute is unconditionally true for an instance that exists. This server
// reports `authbridge exec` instances, which are reached at the inbound address
// the record names — there is no Gateway API in the picture and so nothing that
// could be absent, which makes "does it have a route" degenerate here. Reporting
// false would be read as "not exposed", which is the opposite of the truth for a
// running instance.
//
// The existence check is delegated to the same lookup the detail endpoint uses,
// so this 404s exactly when that does — including for an mcp instance, which is
// not an agent on either endpoint.
func agentRouteStatusRoute(opts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := lookupInstance(w, r, instances.ProtocolA2A); !ok {
			return
		}
		writeJSON(w, http.StatusOK, RouteStatus{HasRoute: true})
	}
}

// summarize turns an instance record into a listing entry.
//
// The description names the hosted command, which is what distinguishes one
// instance from another to a reader — the generated name is a handle, not a
// hint. Status is "Running" because the file's presence is the claim that it is;
// the record is advisory (see the instances package), so this reports what the
// record says rather than probing the process.
func summarize(inst instances.Instance) ResourceSummary {
	description := summarizeCommand(inst.CommandLine)
	if inst.InboundAddr != "" {
		description = fmt.Sprintf("%s (inbound %s)", description, inst.InboundAddr)
	}

	return ResourceSummary{
		Name:        instanceName(inst),
		Namespace:   inst.Namespace,
		Description: description,
		Status:      instanceStatus,
		Labels: ResourceLabels{
			// The protocol is the one label an instance record really carries.
			Protocol: []string{string(inst.InboundProtocol)},
		},
	}
}

// instanceStatus is the status every reported instance carries. The record's
// presence is the claim that the instance is running; see summarize.
const instanceStatus = "Running"

// localWorkloadType is the workloadType a detail response reports. The backend
// reports the Kubernetes workload behind an agent ("deployment", "knative");
// there is no cluster here, so this says what is actually hosting it rather than
// borrowing a cluster word that would misdescribe it.
const localWorkloadType = "authbridge-exec"

// protocolLabel is the label key a detail response records the inbound protocol
// under. The CLI's renderer reads protocol.rossoctl.io/<proto> keys, so an
// instance's protocol shows up in `rossoctl agents get` without a special case.
const protocolLabel = "protocol.rossoctl.io/"

// instanceName is the name to report an instance under. A record with no name
// would otherwise show as a blank row or an unaddressable entry; the ID is
// always present, so it stands in as the handle.
func instanceName(inst instances.Instance) string {
	if inst.Name != "" {
		return inst.Name
	}
	return inst.ID
}

// detail turns an instance record into a detail response.
//
// Every field the record carries is reported, under the spec and status keys the
// CLI's renderer already reads, so `rossoctl agents get` shows a local instance
// without knowing it is local. The addresses are the useful part: they are what
// a caller needs to reach the instance, and nothing else publishes them.
//
// Absent fields are omitted rather than emitted as empty strings, so a reader can
// tell "no inbound listener" from "an inbound listener at nowhere" — the same
// distinction the record itself makes.
func detail(inst instances.Instance) ResourceDetail {
	spec := map[string]any{
		"description": summarizeCommand(inst.CommandLine),
		"command":     inst.CommandLine,
	}
	for k, v := range map[string]string{
		"inboundAddr":   inst.InboundAddr,
		"sessionAddr":   inst.SessionAddr,
		"adminAddr":     inst.AdminAddr,
		"containerName": inst.ContainerName,
	} {
		if v != "" {
			spec[k] = v
		}
	}

	// One Ready condition, so a renderer that shows a conditions table has a row
	// rather than an empty section. The record's presence is the evidence.
	status := map[string]any{
		"conditions": []any{map[string]any{
			"type":    "Ready",
			"status":  "True",
			"reason":  "InstanceRecorded",
			"message": "an authbridge exec instance record is present for this name",
		}},
	}
	if inst.PID != 0 {
		status["pid"] = inst.PID
	}

	// The record's ID, not the name: the name is already in metadata.name, and
	// the ID is what distinguishes two runs that reused one name.
	uid := inst.ID

	return ResourceDetail{
		Metadata: ResourceMetadata{
			Name:      instanceName(inst),
			Namespace: inst.Namespace,
			Labels: map[string]string{
				protocolLabel + string(inst.InboundProtocol): "true",
			},
			Annotations: map[string]string{},
			UID:         &uid,
		},
		Spec:         spec,
		Status:       status,
		WorkloadType: localWorkloadType,
		ReadyStatus:  instanceStatus,
	}
}

// maxDescriptionLen bounds a listing description. A hosted command can be an
// inline shell script of any length; a listing has room for a label, so the rest
// is elided rather than shipped for a UI to truncate mid-row.
const maxDescriptionLen = 120

// summarizeCommand renders a command line as a single-line label.
//
// Runs of whitespace — including the newlines of an inline shell script — are
// collapsed to single spaces, since a multi-line description would break a
// one-row-per-instance listing. The result is elided at maxDescriptionLen.
func summarizeCommand(argv []string) string {
	// strings.Fields splits on any whitespace run, so joining its output both
	// collapses runs and drops leading and trailing space.
	line := strings.Join(strings.Fields(strings.Join(argv, " ")), " ")
	if len(line) > maxDescriptionLen {
		// Cut at maxDescriptionLen bytes, then back off to the last rune
		// boundary so a multi-byte character is not split in half.
		cut := line[:maxDescriptionLen]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		line = strings.TrimRight(cut, " ") + "..."
	}
	return line
}

// unimplementedHandler answers every placeholder route with 500 UNIMPLEMENTED.
func unimplementedHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": unimplementedMessage})
}

// writeJSON writes v as a JSON body with the given status. An encoding failure
// cannot be reported to the client once the header is out, so it is dropped;
// every value written here is a fixed-shape struct or map that cannot fail.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
