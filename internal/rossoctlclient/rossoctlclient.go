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

// NewClient builds a Rossoctl backend for ctx: an HTTP apiclient.Client for the
// context's server and bearer token.
//
// Every context type reaches the API over HTTP, including TypeCortex. A cortex
// context once had its own file-backed client reading agents.json directly; that
// backend is gone, and a cortex is now reached the same way as any other server —
// by pointing the context at a `rossoctl cortex serve` address.
//
// Verbose request logging is not wired here: callers that want it can type-
// assert the result to *apiclient.Client and set its Logf field.
func NewClient(ctx *config.Context) Rossoctl {
	return &apiclient.Client{
		BaseURL:     ctx.Server,
		BearerToken: ctx.BearerToken,
	}
}
