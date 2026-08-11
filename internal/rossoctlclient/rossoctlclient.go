// Package rossoctlclient defines the Rossoctl interface: the set of operations
// the command layer needs from a backend.
//
// The interface mirrors the public methods of apiclient.Client and reuses that
// package's request/response types, so that client satisfies Rossoctl without
// any adaptation. It stays an interface rather than a concrete type so tests can
// substitute a fake, and so a second backend could be added without touching
// every caller.
package rossoctlclient

import (
	"context"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/inprocess"
)

// Rossoctl is the backend contract used by the command layer. Its methods are
// exactly the public methods of apiclient.Client.
type Rossoctl interface {
	// GetAuthConfig fetches the server's auth configuration.
	GetAuthConfig(ctx context.Context) (*apiclient.AuthConfig, error)

	// GetAuthStatus fetches the server's authentication status (GET /auth/status).
	GetAuthStatus(ctx context.Context) (*apiclient.AuthStatus, error)

	// GetUserInfo fetches the current user (GET /auth/me); returns a guest user
	// when unauthenticated rather than erroring.
	GetUserInfo(ctx context.Context) (*apiclient.UserInfo, error)

	// GetPlatformStatus fetches aggregated platform status
	// (GET /config/platform-status).
	GetPlatformStatus(ctx context.Context) (*apiclient.PlatformStatus, error)

	// ListAgents lists agents in the given namespace (empty => server default).
	ListAgents(ctx context.Context, namespace string) (*apiclient.AgentListResponse, error)

	// GetAgent fetches a single agent by namespace and name.
	GetAgent(ctx context.Context, namespace, name string) (*apiclient.AgentDetail, error)

	// GetAgentRouteStatus reports whether an HTTPRoute exposes the agent.
	GetAgentRouteStatus(ctx context.Context, namespace, name string) (*apiclient.RouteStatus, error)

	// GetAgentCard fetches the agent's A2A card, which the backend proxies from
	// the running agent.
	GetAgentCard(ctx context.Context, namespace, name string) (*apiclient.AgentCard, error)

	// GetAgentIdentityConfig fetches the agent's AuthBridge configuration: its
	// mode and the inbound/outbound plugin pipeline.
	GetAgentIdentityConfig(ctx context.Context, namespace, name string) (*apiclient.AgentIdentityConfig, error)

	// DeleteAgent deletes an agent by namespace and name.
	DeleteAgent(ctx context.Context, namespace, name string) (*apiclient.DeleteResponse, error)

	// CreateAgent creates an agent from the given request.
	CreateAgent(ctx context.Context, req *apiclient.CreateAgentRequest) (*apiclient.CreateAgentResponse, error)

	// ListTools lists tools in the given namespace (empty => server default).
	ListTools(ctx context.Context, namespace string) (*apiclient.ToolListResponse, error)

	// GetTool fetches a single tool by namespace and name.
	GetTool(ctx context.Context, namespace, name string) (*apiclient.ToolDetail, error)

	// DeleteTool deletes a tool by namespace and name.
	DeleteTool(ctx context.Context, namespace, name string) (*apiclient.DeleteResponse, error)

	// CreateTool creates a tool from the given request.
	CreateTool(ctx context.Context, req *apiclient.CreateToolRequest) (*apiclient.CreateToolResponse, error)

	// ListNamespaces lists namespaces; enabledOnly restricts to rossoctl-enabled
	// namespaces (the server default).
	ListNamespaces(ctx context.Context, enabledOnly bool) (*apiclient.NamespaceListResponse, error)
}

// Compile-time assertion that the HTTP client implements Rossoctl.
var _ Rossoctl = (*apiclient.Client)(nil)

// CortexContextName is the context name that selects the in-process transport.
// A context so named is answered by a serve handler inside this process instead
// of over a socket, so its commands work without a `rossoctl cortex serve`
// daemon running.
//
// A name is the marker because nothing else distinguishes such a context: a
// cortex is reached over HTTP like any other server, and the config's type field
// is "api" for every context every command creates. The cost of that convention
// is that a context named "cortex" pointed at a remote server is answered locally
// instead — which --verbose reports on every request, since the transport names
// itself in the log.
const CortexContextName = "cortex"

// NewClient builds a Rossoctl backend for ctx: an apiclient.Client for the
// context's server and bearer token.
//
// There is one implementation for every context. A context named "cortex" gets a
// transport that answers from a serve handler in this process rather than over a
// socket, but it is the same apiclient.Client either way, so URL construction,
// status handling, and JSON decoding are shared and the two paths cannot
// disagree above the transport. A cortex context once had its own file-backed
// client reading agents.json directly; that backend is gone, and substituting a
// transport is what replaced it.
//
// The bearer token is set even for a cortex context. The serve handler ignores
// credentials entirely, but dropping the token here would silently break a
// context later repointed at a real server.
//
// An error is returned only when the in-process handler cannot be built — a
// context with no server, or an unreadable instances directory. Falling back to
// dialing in that case would send the command to a server the user did not ask
// for, so the failure is reported instead.
//
// Verbose request logging is not wired here: callers that want it can type-
// assert the result to *apiclient.Client and set its Logf field.
func NewClient(ctx *config.Context) (Rossoctl, error) {
	client := &apiclient.Client{
		BaseURL:     ctx.Server,
		BearerToken: ctx.BearerToken,
	}

	if ctx.Name == CortexContextName {
		httpClient, err := inprocess.New(ctx)
		if err != nil {
			return nil, err
		}
		client.HTTPClient = httpClient
	}

	// Any other context keeps HTTPClient nil, so apiclient supplies its own
	// default with the timeout a real network call needs.
	return client, nil
}
