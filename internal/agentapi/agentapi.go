// Package agentapi defines the wire types of the rossoctl backend's agent and
// tool endpoints: the JSON shapes, and nothing else.
//
// It exists because two packages sit on opposite ends of the same wire and had
// each grown their own copy of these structs. internal/apiclient decodes the
// backend's responses; internal/serve encodes them, standing in for the backend
// so a UI or CLI can be run against local `authbridge exec` instances. Two copies
// of one contract only agree by coincidence: a renamed field or a mistyped tag on
// one side is caught only if some test happens to exercise both, and reading
// either copy tells you nothing about whether the other matches.
//
// Deliberately not here:
//
//   - HTTP. No client, no handlers, no status codes. Both users need the shapes
//     without inheriting each other's transport.
//   - Request bodies. CreateAgentRequest and friends stay in apiclient; only the
//     server-to-client direction is shared, because only that direction has two
//     implementations to keep honest.
//   - instances.Instance, the on-disk record `authbridge exec` writes. That is a
//     private format between the writer and the local server, in snake_case and
//     carrying PIDs and container names. The server converts it into these types
//     (see serve.detail); hoisting it here would couple every client to a disk
//     layout it must never see.
//
// A caveat on what this does and does not guarantee. The real backend is a
// separate Python service, and its published OpenAPI document is the authority
// for these shapes. Sharing a type keeps the two halves of *this* repository
// honest with each other; it cannot detect a drift from the backend itself. That
// still has to be checked against the published schema.
//
// Names follow the backend's schema names — AgentDetail mirrors its
// AgentDetailResponse — so a field can be traced from the OpenAPI document to
// here without a translation step. The tool endpoints return the same shapes, so
// the tool names are aliases rather than copies.
package agentapi

// AgentDetail mirrors the backend's GET /agents/{namespace}/{name} response.
//
// Spec and Status are free-form maps because they are open in the schema: the
// backend fills them from a Kubernetes custom resource, which has no fixed shape
// here. Callers read them opportunistically rather than relying on any key.
type AgentDetail struct {
	Metadata     AgentMetadata       `json:"metadata"`
	Spec         map[string]any      `json:"spec"`
	Status       map[string]any      `json:"status"`
	WorkloadType string              `json:"workloadType"`
	ReadyStatus  string              `json:"readyStatus"`
	Contexts     []ContextAttachment `json:"contexts,omitempty"`

	// Service is absent for anything not fronted by a Kubernetes Service — a
	// local `authbridge exec` instance has no ClusterIP, and naming one nothing
	// would answer on is worse than reporting none.
	Service *ServiceInfo `json:"service"`
}

// ContextAttachment describes a named Context Service resource mounted in an agent.
type ContextAttachment struct {
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly"`
	ClaimName string `json:"claimName,omitempty"`
}

// ToolDetail mirrors the backend's GET /tools/{namespace}/{name} response, which
// has the same shape as an agent's. An alias rather than a copy, so the two
// cannot drift and one set of renderer helpers serves both.
type ToolDetail = AgentDetail

// AgentMetadata is the metadata block of a detail response.
//
// CreationTimestamp and UID are nullable in the schema and are pointers here for
// that reason: a resource with no recorded start time is distinguishable from one
// timestamped at the zero value.
type AgentMetadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp *string           `json:"creationTimestamp"`
	UID               *string           `json:"uid"`
}

// ServiceInfo is the optional service block of a detail response.
type ServiceInfo struct {
	Name      string        `json:"name"`
	Type      string        `json:"type"`
	ClusterIP string        `json:"clusterIP"`
	Ports     []ServicePort `json:"ports"`
}

// ServicePort is one port of a resource's Service.
type ServicePort struct {
	Name string `json:"name"`
	Port int    `json:"port"`

	// TargetPort is a Kubernetes IntOrString: it is either a port number or the
	// name of a port in the pod spec. Kept as any because both are valid and the
	// renderer only formats it.
	TargetPort any `json:"targetPort"`
}

// AgentSummary is one entry in the GET /agents response.
//
// WorkloadType and CreatedAt are nullable in the schema, and null in practice for
// anything that is not a cluster workload.
type AgentSummary struct {
	Name         string         `json:"name"`
	Namespace    string         `json:"namespace"`
	Description  string         `json:"description"`
	Status       string         `json:"status"`
	Labels       ResourceLabels `json:"labels"`
	WorkloadType *string        `json:"workloadType"`
	CreatedAt    *string        `json:"createdAt"`
}

// ToolSummary is one entry in the GET /tools response, which has the same shape
// as an agent's. An alias, for the same reason as ToolDetail.
type ToolSummary = AgentSummary

// ResourceLabels is the labels block of a summary: the backend's decoding of the
// protocol/framework/type labels, rather than the raw label map.
//
// Framework and Type are nullable; Protocol is a list because a resource may
// speak more than one.
type ResourceLabels struct {
	Protocol  []string `json:"protocol"`
	Framework *string  `json:"framework"`
	Type      *string  `json:"type"`
}

// RouteStatus is the GET /agents/{namespace}/{name}/route-status response,
// reporting whether an HTTPRoute exposes the agent.
//
// HasRoute has no omitempty: false is a meaningful answer here, and omitempty
// would make it indistinguishable from a field the server never set.
type RouteStatus struct {
	HasRoute bool `json:"hasRoute"`
}

// AgentCard is the GET /chat/{namespace}/{name}/agent-card response: an agent's
// A2A card, as reshaped by the backend.
//
// These are exactly the fields the backend's response model declares. The A2A
// card an agent serves carries more — protocolVersion, preferredTransport,
// defaultInputModes — but the backend reshapes the card before answering, so
// those never reach a client. Note Streaming is flattened to the top level here,
// where the A2A card nests it under "capabilities".
//
// Only Name, Version and URL are required; Description and Skills are optional
// and Streaming defaults to false.
type AgentCard struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Version     string           `json:"version"`
	URL         string           `json:"url"`
	Streaming   bool             `json:"streaming"`
	Skills      []AgentCardSkill `json:"skills"`
}

// AgentCardSkill is one entry in an AgentCard's skills list.
type AgentCardSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples"`
}
