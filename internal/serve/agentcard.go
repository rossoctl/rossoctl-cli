package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/rossoctl/rossoctl-cli/internal/instances"
)

// agentCardPath is the well-known location an A2A agent serves its card from.
// Named here rather than inlined so it matches the backend's A2A_AGENT_CARD_PATH
// by inspection.
const agentCardPath = "/.well-known/agent-card.json"

// agentCardTimeout bounds the fetch from the hosted agent.
//
// Ten seconds, matching the backend's httpx timeout. The agent is a process on
// this host reached over loopback, so a healthy one answers in microseconds; the
// budget is for an agent that accepted the connection and then stalled, which
// must not hang this server's own client indefinitely.
const agentCardTimeout = 10 * time.Second

// cardFetcher fetches the card document at the given URL. A variable so a test can
// answer without a live agent, following lister and getter above; production
// always uses the real HTTP client.
var cardFetcher = fetchCardDocument

// agentCardResponse mirrors the backend's AgentCardResponse (see
// rossoctl/backend/app/routers/chat.py): the reshaped card its /chat endpoint
// returns, not the A2A card the agent serves.
//
// Deliberately a local type rather than agentapi.AgentCard. The two are the same
// shape today, and this server exists to stand in for that backend, but the
// conversion below is this package's own reading of a spec card — keeping the
// struct here means the wire test proves the shapes agree rather than assuming it
// by sharing a definition.
//
// Description is a pointer because the backend's field is Optional[str] with no
// default: an agent that declares no description yields null, which a renderer can
// tell from the empty string an agent explicitly served.
type agentCardResponse struct {
	Name        string           `json:"name"`
	Description *string          `json:"description"`
	Version     string           `json:"version"`
	URL         string           `json:"url"`
	Streaming   bool             `json:"streaming"`
	Skills      []map[string]any `json:"skills"`
}

// agentCardRoute serves GET /chat/{namespace}/{name}/agent-card by fetching the
// card from the agent this server has a record for.
//
// Unlike the other endpoints here, this one cannot be answered from the record
// alone: a card is the agent's own description of itself, so it has to be asked.
// That makes this the one handler that makes an outbound request, and the reason
// the failure modes below are spelled out — a caller has to tell "no such
// instance" from "the agent is not answering".
//
// The record is resolved through the same lookup the detail endpoint uses, so this
// 404s exactly when that does, including for an mcp instance: a card is an A2A
// concept and an mcp instance is not an agent on any of these endpoints.
func agentCardRoute(opts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, ok := lookupInstance(w, r, instances.ProtocolA2A)
		if !ok {
			return
		}

		// An instance with no inbound listener has no address to ask. That is a 503
		// rather than a 404: the instance exists, so reporting it absent would
		// contradict the detail endpoint, and it is not the caller's request that is
		// wrong.
		if inst.InboundAddr == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"detail": fmt.Sprintf("instance %q has no inbound address to fetch an agent card from", inst.Name),
			})
			return
		}

		cardURL := "http://" + inst.InboundAddr + agentCardPath
		body, status, err := cardFetcher(r.Context(), cardURL)
		if err != nil {
			// 503, matching the backend's httpx.RequestError branch: the agent could
			// not be reached, which is the agent's state and not a fault in this
			// server. The address is named because on a host running several
			// instances it is what identifies which one is down.
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"detail": fmt.Sprintf("failed to connect to agent at %s: %v", inst.InboundAddr, err),
			})
			return
		}
		if status < 200 || status >= 300 {
			// The agent's own status is passed through, as the backend does for an
			// HTTPStatusError. An agent that answers 404 at the well-known path is
			// not an A2A agent, and flattening that to 500 would hide which of the
			// two hops refused.
			writeJSON(w, status, map[string]string{
				"detail": fmt.Sprintf("failed to fetch agent card from %s: agent returned %d", inst.InboundAddr, status),
			})
			return
		}

		card, cardURLFromDoc, err := parseAgentCard(body)
		if err != nil {
			// A body that is not a card is the agent's fault, not the caller's, but
			// it is a real answer rather than a connection failure — so 502 rather
			// than the 503 above, distinguishing "no answer" from "bad answer".
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"detail": fmt.Sprintf("agent at %s served an unreadable agent card: %v", inst.InboundAddr, err),
			})
			return
		}

		writeJSON(w, http.StatusOK, convertAgentCard(card, cardURLFromDoc, inst.InboundAddr))
	}
}

// fetchCardDocument GETs the card document, returning the body and status. The
// body is read even for an error status so a caller can report what was served.
//
// The read is capped: this server is asking a process it does not control, and an
// agent streaming an endless body should fail rather than consume this server's
// memory. A card is a few kilobytes, so a megabyte is generous.
func fetchCardDocument(ctx context.Context, cardURL string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, agentCardTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// parseAgentCard reads a card document into the a2a-go type, returning the card
// and the endpoint URL the document declared.
//
// The URL is returned separately rather than read back off the card because the
// two A2A revisions disagree about where it lives. v0.3 puts it at the top level;
// v1.0 moved it into supportedInterfaces[], and a2a.AgentCard has no top-level URL
// field at all. The v0.3 parser used here reconstructs an interface from the flat
// url — but only when the document also carries preferredTransport, and it drops
// the URL silently when it does not. A minimal agent omits that field, so reading
// the flat url directly is what keeps this from serving a card with no endpoint.
func parseAgentCard(body []byte) (*a2a.AgentCard, string, error) {
	card, err := a2av0.NewAgentCardParser()(body)
	if err != nil {
		return nil, "", err
	}

	// The declared endpoint, preferring what the document said literally over the
	// parser's reconstruction of it.
	var flat struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &flat); err != nil {
		return nil, "", err
	}
	declared := flat.URL
	if declared == "" && len(card.SupportedInterfaces) > 0 {
		declared = card.SupportedInterfaces[0].URL
	}
	return card, declared, nil
}

// convertAgentCard reshapes a spec card into the backend's AgentCardResponse,
// pointing its URL at the address this server reaches the agent on.
//
// The fallbacks mirror the backend's: a card missing a version reports "unknown"
// rather than an empty string, because the field is required in the response
// schema and a reader seeing a blank cannot tell it from a rendering fault.
func convertAgentCard(card *a2a.AgentCard, declaredURL, inboundAddr string) agentCardResponse {
	out := agentCardResponse{
		Name:      card.Name,
		Version:   card.Version,
		URL:       rewriteCardHost(declaredURL, inboundAddr),
		Streaming: card.Capabilities.Streaming,

		// An empty array rather than null: the backend's field defaults to [], and a
		// renderer that distinguishes "no skills" from "field absent" would be
		// reading a difference this server does not intend.
		Skills: make([]map[string]any, 0, len(card.Skills)),
	}
	if card.Version == "" {
		out.Version = "unknown"
	}
	if card.Description != "" {
		d := card.Description
		out.Description = &d
	}

	for _, s := range card.Skills {
		skill := map[string]any{
			"id":          s.ID,
			"name":        s.Name,
			"description": s.Description,
			"examples":    s.Examples,
		}
		// Tags are carried too, though the backend's conversion drops them. Its
		// skills field is an open dict, so this adds to that shape rather than
		// departing from it, and the CLI's card renderer shows tags — dropping them
		// here would make the same agent look different through this server.
		if len(s.Tags) > 0 {
			skill["tags"] = s.Tags
		}
		out.Skills = append(out.Skills, skill)
	}
	return out
}

// rewriteCardHost replaces the host and port of the card's declared URL with the
// address this server reaches the agent on, keeping the rest of the URL.
//
// The path is kept because an agent behind a prefix declares it there, and the
// caller needs the path to talk to it. The host is replaced because the agent's
// own view of its address is routinely useless to a caller: an A2A server
// typically declares whatever it was bound to, so a card served by a container
// says 0.0.0.0 or an in-cluster name, neither of which resolves here. The record's
// inbound address is where this host actually reaches it.
//
// A URL that will not parse, or that declares no host, falls back to the inbound
// address with the well-known path dropped — an unusable URL is worse than a
// plain one, and a caller can still reach the agent at its address.
func rewriteCardHost(declaredURL, inboundAddr string) string {
	fallback := "http://" + inboundAddr

	if declaredURL == "" {
		return fallback
	}
	u, err := url.Parse(declaredURL)
	if err != nil || u.Host == "" {
		return fallback
	}

	// Only the host:port is replaced. Scheme, path, query and fragment are the
	// agent's own and are preserved; userinfo is dropped with the authority it
	// belonged to, since a credential scoped to the agent's declared host is not
	// one to replay at a different one.
	u.Host = inboundAddr
	u.User = nil

	// A card served over https declares https, but the record's inbound address is
	// a plain loopback listener this server reaches over http. Keeping https would
	// send the caller somewhere nothing is listening.
	if u.Scheme != "http" {
		u.Scheme = "http"
	}
	return u.String()
}
