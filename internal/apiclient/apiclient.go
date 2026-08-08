// Package apiclient is a thin HTTP client for the Rossoctl backend API.
//
// Like the other internal packages it is free of Cobra: it takes a base
// server URI and returns decoded results (or errors), so it can be tested
// against an httptest.Server without involving the command tree.
package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a Rossoctl API server rooted at BaseURL.
type Client struct {
	// BaseURL is the API root, e.g. http://host:8080/api/v1/. A trailing
	// slash is optional; paths are joined relative to it.
	BaseURL string

	// HTTPClient is used for requests. If nil, a client with a sensible
	// timeout is used.
	HTTPClient *http.Client

	// BearerToken, if non-empty, is sent as an Authorization: Bearer header on
	// every request.
	BearerToken string

	// Logf, if set, is called to log each HTTP request and its outcome.
	// The command layer wires this to stderr when --verbose is given; when
	// nil, no logging happens. Kept as a plain function so this package
	// stays free of any logging or CLI dependency.
	Logf func(format string, args ...any)
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// StatusError is returned for any response outside 2xx. It carries the status
// code so callers can react to a particular one — the command layer suggests
// signing in on a 401 — rather than matching on the message text.
//
// The Error string is unchanged from the plain fmt.Errorf this replaced, so
// existing output and any test asserting on it still hold. No advice is added
// here: this package is free of Cobra and does not know the binary's name or
// which of its commands to recommend.
type StatusError struct {
	// Endpoint is the full URL that was requested.
	Endpoint string

	// StatusCode is the HTTP status returned.
	StatusCode int

	// Body is the response body, trimmed and truncated, or the status line
	// when the body was empty.
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.Endpoint, e.StatusCode, e.Body)
}

// AuthConfig mirrors the backend's AuthConfigResponse (GET /auth/config).
// Pointer fields are used for the optional values so that "absent" (null)
// is distinguishable from "empty string" when rendering.
type AuthConfig struct {
	Enabled     bool    `json:"enabled"`
	KeycloakURL *string `json:"keycloak_url"`
	Realm       *string `json:"realm"`
	ClientID    *string `json:"client_id"`
	RedirectURI *string `json:"redirect_uri"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// resolve joins ref onto BaseURL, treating BaseURL as a directory (so the
// last path segment of the base is preserved rather than replaced).
func (c *Client) resolve(ref string) (string, error) {
	base := c.BaseURL
	if base == "" {
		return "", fmt.Errorf("server URI is empty")
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid server URI %q: %w", c.BaseURL, err)
	}
	refURL, err := url.Parse(strings.TrimPrefix(ref, "/"))
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", ref, err)
	}
	return baseURL.ResolveReference(refURL).String(), nil
}

// getJSON performs a GET on the resolved path and decodes the JSON body
// into out.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, out)
}

// deleteJSON performs a DELETE on the resolved path and decodes the JSON body
// into out.
func (c *Client) deleteJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodDelete, path, out)
}

// postJSON performs a POST on the resolved path with body marshaled as JSON
// and decodes the JSON response into out.
func (c *Client) postJSON(ctx context.Context, path string, body, out any) error {
	return c.requestJSON(ctx, http.MethodPost, path, body, out)
}

// doJSON issues a bodyless request with the given method.
func (c *Client) doJSON(ctx context.Context, method, path string, out any) error {
	return c.requestJSON(ctx, method, path, nil, out)
}

// requestJSON issues a request with the given method (and optional JSON body),
// applies auth and logging, checks the status, and decodes the JSON response
// into out.
func (c *Client) requestJSON(ctx context.Context, method, path string, body, out any) error {
	endpoint, err := c.resolve(path)
	if err != nil {
		return err
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reqBody = strings.NewReader(string(data))
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}

	c.logf("%s %s", method, endpoint)
	start := time.Now()
	resp, err := c.httpClient().Do(req)
	if err != nil {
		c.logf("%s %s failed after %s: %v", method, endpoint, time.Since(start).Round(time.Millisecond), err)
		return fmt.Errorf("requesting %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	c.logf("%s %s -> %s (%s)", method, endpoint, resp.Status, time.Since(start).Round(time.Millisecond))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return &StatusError{Endpoint: endpoint, StatusCode: resp.StatusCode, Body: msg}
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", endpoint, err)
	}
	return nil
}

// GetAuthConfig fetches GET /auth/config from the server.
func (c *Client) GetAuthConfig(ctx context.Context) (*AuthConfig, error) {
	var cfg AuthConfig
	if err := c.getJSON(ctx, "auth/config", &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// AuthStatus mirrors the backend's AuthStatusResponse (GET /auth/status): what
// the web UI's Current Session panel reads for authentication state. Optional
// values are pointers so "absent" (null) is distinct from an empty string.
type AuthStatus struct {
	Enabled       bool    `json:"enabled"`
	Authenticated bool    `json:"authenticated"`
	KeycloakURL   *string `json:"keycloak_url"`
	Realm         *string `json:"realm"`
	ClientID      *string `json:"client_id"`
}

// GetAuthStatus fetches GET /auth/status from the server.
func (c *Client) GetAuthStatus(ctx context.Context) (*AuthStatus, error) {
	var status AuthStatus
	if err := c.getJSON(ctx, "auth/status", &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// UserInfo mirrors the backend's UserInfoResponse (GET /auth/me): the current
// user shown in the web UI's Current Session panel. When auth is disabled or
// the request is unauthenticated the server returns a guest user.
type UserInfo struct {
	Username      string   `json:"username"`
	Email         string   `json:"email"`
	Roles         []string `json:"roles"`
	Authenticated bool     `json:"authenticated"`
}

// GetUserInfo fetches GET /auth/me from the server. This endpoint uses optional
// auth: it returns a guest user rather than an error when unauthenticated.
func (c *Client) GetUserInfo(ctx context.Context) (*UserInfo, error) {
	var info UserInfo
	if err := c.getJSON(ctx, "auth/me", &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ComponentStatus mirrors the backend's ComponentStatus: the health of one
// platform component (Istio, Keycloak, SPIRE, etc.). status is one of
// "Ready", "Degraded", "Missing", or "Unknown".
type ComponentStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// RegistryBuildInfo mirrors the backend's RegistryBuildInfo: the container
// registry endpoint and Shipwright ClusterBuildStrategy availability.
type RegistryBuildInfo struct {
	ClusterBuildStrategyPresent bool     `json:"clusterBuildStrategyPresent"`
	ClusterBuildStrategies      []string `json:"clusterBuildStrategies"`
	RegistryEndpoint            string   `json:"registryEndpoint"`
}

// PlatformStatus mirrors the backend's PlatformStatusResponse
// (GET /config/platform-status): the data behind the web UI's Platform Status
// panel.
type PlatformStatus struct {
	Components []ComponentStatus `json:"components"`
	Registry   RegistryBuildInfo `json:"registry"`
}

// GetPlatformStatus fetches GET /config/platform-status from the server.
func (c *Client) GetPlatformStatus(ctx context.Context) (*PlatformStatus, error) {
	var status PlatformStatus
	if err := c.getJSON(ctx, "config/platform-status", &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// ResourceLabels mirrors the backend's ResourceLabels model.
type ResourceLabels struct {
	Protocol  []string `json:"protocol"`
	Framework *string  `json:"framework"`
	Type      *string  `json:"type"`
}

// AgentSummary mirrors the backend's AgentSummary model (one entry in the
// GET /agents response).
type AgentSummary struct {
	Name         string         `json:"name"`
	Namespace    string         `json:"namespace"`
	Description  string         `json:"description"`
	Status       string         `json:"status"`
	Labels       ResourceLabels `json:"labels"`
	WorkloadType *string        `json:"workloadType"`
	CreatedAt    *string        `json:"createdAt"`
}

// AgentListResponse mirrors the backend's AgentListResponse model.
type AgentListResponse struct {
	Items []AgentSummary `json:"items"`
}

// ListAgents fetches GET /agents for the given namespace. If namespace is
// empty the server's default namespace is used.
func (c *Client) ListAgents(ctx context.Context, namespace string) (*AgentListResponse, error) {
	path := "agents"
	if namespace != "" {
		path += "?namespace=" + url.QueryEscape(namespace)
	}

	var resp AgentListResponse
	if err := c.getJSON(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// AgentMetadata is the metadata block of an agent detail response.
type AgentMetadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp *string           `json:"creationTimestamp"`
	UID               *string           `json:"uid"`
}

// ServicePort is one port of an agent's Service.
type ServicePort struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	TargetPort any    `json:"targetPort"` // may be int or string
}

// ServiceInfo is the optional service block of an agent detail response.
type ServiceInfo struct {
	Name      string        `json:"name"`
	Type      string        `json:"type"`
	ClusterIP string        `json:"clusterIP"`
	Ports     []ServicePort `json:"ports"`
}

// AgentDetail mirrors the backend's GET /agents/{namespace}/{name} response.
// spec and status are workload-shaped and free-form, so they are kept as maps
// and read opportunistically by the renderer.
type AgentDetail struct {
	Metadata     AgentMetadata  `json:"metadata"`
	Spec         map[string]any `json:"spec"`
	Status       map[string]any `json:"status"`
	WorkloadType string         `json:"workloadType"`
	ReadyStatus  string         `json:"readyStatus"`
	Service      *ServiceInfo   `json:"service"`
}

// GetAgent fetches GET /agents/<namespace>/<name>.
func (c *Client) GetAgent(ctx context.Context, namespace, name string) (*AgentDetail, error) {
	path := "agents/" + url.PathEscape(namespace) + "/" + url.PathEscape(name)

	var detail AgentDetail
	if err := c.getJSON(ctx, path, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// DeleteResponse mirrors the backend's DeleteResponse model.
type DeleteResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// DeleteAgent issues DELETE /agents/<namespace>/<name>.
func (c *Client) DeleteAgent(ctx context.Context, namespace, name string) (*DeleteResponse, error) {
	path := "agents/" + url.PathEscape(namespace) + "/" + url.PathEscape(name)

	var resp DeleteResponse
	if err := c.deleteJSON(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// EnvVar is one environment variable in a CreateAgentRequest.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CreateAgentRequest is the subset of the backend's CreateAgentRequest that
// the CLI populates. Fields the server defaults are omitted; only what we set
// is sent. deploymentMethod selects image vs source; workloadType selects
// deployment|statefulset|job|sandbox.
type CreateAgentRequest struct {
	Name             string   `json:"name"`
	Namespace        string   `json:"namespace"`
	DeploymentMethod string   `json:"deploymentMethod"`
	WorkloadType     string   `json:"workloadType"`
	EnvVars          []EnvVar `json:"envVars,omitempty"`

	// Image deployment fields.
	ContainerImage  string `json:"containerImage,omitempty"`
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// Source build fields.
	GitURL    string `json:"gitUrl,omitempty"`
	GitPath   string `json:"gitPath,omitempty"`
	GitBranch string `json:"gitBranch,omitempty"`

	// CreateHTTPRoute asks the server to create an HTTPRoute exposing the
	// agent. The server's own default is false, matching this zero value.
	//
	// Deliberately without omitempty, unlike the fields above: a false bool is
	// indistinguishable from an absent one, so omitempty would drop an explicit
	// --createHttpRoute=false and leave the server to apply its default. That
	// default agrees today, but sending what the caller asked for should not
	// depend on the two staying in agreement.
	CreateHTTPRoute bool `json:"createHttpRoute"`
}

// CreateAgentResponse mirrors the backend's CreateAgentResponse model.
type CreateAgentResponse struct {
	Success   bool   `json:"success"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Message   string `json:"message"`
}

// CreateAgent issues POST /agents with the given request body.
func (c *Client) CreateAgent(ctx context.Context, req *CreateAgentRequest) (*CreateAgentResponse, error) {
	var resp CreateAgentResponse
	if err := c.postJSON(ctx, "agents", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ToolSummary mirrors the backend's ToolSummary model (one entry in the
// GET /tools response). It has the same shape as AgentSummary.
type ToolSummary struct {
	Name         string         `json:"name"`
	Namespace    string         `json:"namespace"`
	Description  string         `json:"description"`
	Status       string         `json:"status"`
	Labels       ResourceLabels `json:"labels"`
	WorkloadType *string        `json:"workloadType"`
	CreatedAt    *string        `json:"createdAt"`
}

// ToolListResponse mirrors the backend's ToolListResponse model.
type ToolListResponse struct {
	Items []ToolSummary `json:"items"`
}

// ListTools fetches GET /tools for the given namespace. If namespace is empty
// the server's default namespace is used.
func (c *Client) ListTools(ctx context.Context, namespace string) (*ToolListResponse, error) {
	path := "tools"
	if namespace != "" {
		path += "?namespace=" + url.QueryEscape(namespace)
	}

	var resp ToolListResponse
	if err := c.getJSON(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ToolDetail mirrors the backend's GET /tools/{namespace}/{name} response. It
// has the same shape as AgentDetail (metadata + free-form spec/status +
// workloadType/readyStatus + optional service), so it is an alias to reuse the
// same renderer helpers.
type ToolDetail = AgentDetail

// GetTool fetches GET /tools/<namespace>/<name>.
func (c *Client) GetTool(ctx context.Context, namespace, name string) (*ToolDetail, error) {
	path := "tools/" + url.PathEscape(namespace) + "/" + url.PathEscape(name)

	var detail ToolDetail
	if err := c.getJSON(ctx, path, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// DeleteTool issues DELETE /tools/<namespace>/<name>.
func (c *Client) DeleteTool(ctx context.Context, namespace, name string) (*DeleteResponse, error) {
	path := "tools/" + url.PathEscape(namespace) + "/" + url.PathEscape(name)

	var resp DeleteResponse
	if err := c.deleteJSON(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateServicePort mirrors the backend's ServicePort model (an entry in a
// CreateToolRequest's servicePorts). It is a distinct type from ServicePort
// (used for GET responses) because the request form has an integer
// targetPort and an explicit protocol.
type CreateServicePort struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	TargetPort int    `json:"targetPort"`
	Protocol   string `json:"protocol"`
}

// CreateToolRequest is the subset of the backend's CreateToolRequest that the
// CLI populates. Fields the server defaults are omitted; only what we set is
// sent. deploymentMethod selects image vs source; workloadType selects
// deployment|statefulset.
type CreateToolRequest struct {
	Name             string              `json:"name"`
	Namespace        string              `json:"namespace"`
	DeploymentMethod string              `json:"deploymentMethod"`
	WorkloadType     string              `json:"workloadType"`
	EnvVars          []EnvVar            `json:"envVars,omitempty"`
	ServicePorts     []CreateServicePort `json:"servicePorts,omitempty"`

	// Image deployment fields.
	ContainerImage  string `json:"containerImage,omitempty"`
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// Source build fields.
	GitURL    string `json:"gitUrl,omitempty"`
	GitPath   string `json:"gitPath,omitempty"`
	GitBranch string `json:"gitBranch,omitempty"`

	// CreateHTTPRoute asks the server to create an HTTPRoute exposing the tool.
	// The server's own default is false, matching this zero value.
	//
	// Deliberately without omitempty, for the same reason as the identically
	// named field on CreateAgentRequest: a false bool is indistinguishable from
	// an absent one, so omitempty would drop an explicit --createHttpRoute=false.
	CreateHTTPRoute bool `json:"createHttpRoute"`
}

// CreateToolResponse mirrors the backend's CreateToolResponse model.
type CreateToolResponse struct {
	Success   bool   `json:"success"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Message   string `json:"message"`
}

// CreateTool issues POST /tools with the given request body.
func (c *Client) CreateTool(ctx context.Context, req *CreateToolRequest) (*CreateToolResponse, error) {
	var resp CreateToolResponse
	if err := c.postJSON(ctx, "tools", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// NamespaceListResponse mirrors the backend's NamespaceListResponse model.
type NamespaceListResponse struct {
	Namespaces []string `json:"namespaces"`
}

// ListNamespaces fetches GET /namespaces. When enabledOnly is true (the
// server default), only rossoctl-enabled namespaces are returned; otherwise
// all namespaces are returned.
func (c *Client) ListNamespaces(ctx context.Context, enabledOnly bool) (*NamespaceListResponse, error) {
	// The server defaults enabled_only to true, so only send the parameter
	// when we want the non-default (false) behavior.
	path := "namespaces"
	if !enabledOnly {
		path += "?enabled_only=false"
	}

	var resp NamespaceListResponse
	if err := c.getJSON(ctx, path, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
