package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

// a2aSendArgs holds the `a2a send` flags.
var a2aSendArgs struct {
	transport         string
	address           string
	withAuthorization bool
	message           string
}

// defaultA2ATransport is the protocol binding used when --transport is not
// given. JSON-RPC is the A2A spec's mandatory binding, so it is the one an
// arbitrary agent is most likely to speak.
const defaultA2ATransport = "jsonrpc"

// a2aTransport maps a --transport value onto an a2a.TransportProtocol.
//
// The a2a constants are spelled for the wire ("JSONRPC", "HTTP+JSON"), which
// makes for awkward CLI input, so the flag accepts case-insensitive friendly
// spellings and this function does the translation. Unknown values are an error
// rather than being passed through: NewAgentInterface would accept any string,
// and the client factory would then fail with a less obvious "no transport"
// message at connect time.
func a2aTransport(name string) (a2a.TransportProtocol, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "jsonrpc", "json-rpc":
		return a2a.TransportProtocolJSONRPC, nil
	case "grpc":
		return a2a.TransportProtocolGRPC, nil
	case "http+json", "httpjson", "rest":
		return a2a.TransportProtocolHTTPJSON, nil
	default:
		return "", fmt.Errorf("unknown --transport %q: want jsonrpc, grpc, or http+json", name)
	}
}

// bearerInterceptor attaches a bearer token to every outbound A2A call.
//
// This is deliberately not a2aclient.AuthInterceptor: that one resolves
// credentials per security scheme, and so only acts when the agent card
// advertises SecuritySchemes and a session ID is attached to the context.
// `--with-authorization` means "send the context's token", which needs to work
// against an agent whose card we may never fetch, so the header is set
// unconditionally instead.
//
// ServiceParams is the transport-agnostic carrier the protocol binding
// serializes (an HTTP header, for the JSON-RPC and HTTP+JSON bindings), so
// setting it here works across transports rather than only over HTTP.
type bearerInterceptor struct {
	a2aclient.PassthroughInterceptor
	token string
}

var _ a2aclient.CallInterceptor = (*bearerInterceptor)(nil)

func (bi *bearerInterceptor) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	if req.ServiceParams == nil {
		req.ServiceParams = a2aclient.ServiceParams{}
	}
	req.ServiceParams["Authorization"] = []string{"Bearer " + bi.token}
	return ctx, nil, nil
}

var a2aSendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send a message to an A2A agent and stream the response",
	Long: `Send a message to an A2A agent and print the events it streams back.

The agent is addressed by --address and spoken to over --transport (jsonrpc by
default). The message text from --message is sent as a single user text part,
and the agent's response is streamed: every event received is printed as it
arrives, so a long-running agent's progress is visible rather than buffered
until it finishes.

With --with-authorization the effective context's bearer token is attached to
each request as an Authorization header, for agents that sit behind an
authenticating proxy.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if a2aSendArgs.address == "" {
			return fmt.Errorf("--address is required")
		}
		if a2aSendArgs.message == "" {
			return fmt.Errorf("--message is required")
		}

		protocol, err := a2aTransport(a2aSendArgs.transport)
		if err != nil {
			return err
		}

		var opts []a2aclient.FactoryOption
		if a2aSendArgs.withAuthorization {
			_, token, err := resolveServer()
			if err != nil {
				return err
			}
			if token == "" {
				return fmt.Errorf("--with-authorization given but the current context has no bearer token; run `rossoctl login` to sign in")
			}
			opts = append(opts, a2aclient.WithCallInterceptors(&bearerInterceptor{token: token}))
		}

		ctx := cmd.Context()
		iface := a2a.NewAgentInterface(a2aSendArgs.address, protocol)
		client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{iface}, opts...)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", a2aSendArgs.address, err)
		}
		defer func() { _ = client.Destroy() }()

		req := &a2a.SendMessageRequest{
			Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(a2aSendArgs.message)),
		}

		out := cmd.OutOrStdout()
		for event, err := range client.SendStreamingMessage(ctx, req) {
			// The iterator reports a failed call by yielding an error; stop at
			// the first one rather than continuing to print, since the stream
			// is not resumable.
			if err != nil {
				return err
			}
			printA2AEvent(out, event)
		}
		return nil
	},
}

// printA2AEvent prints one streamed event as a single line.
//
// Events arrive as one of four concrete types, and the interesting content lives
// in a different place in each, so this switches on the type rather than dumping
// the struct. Content parts are rendered by partText: text as text, structured
// data as JSON, and binary or remote content as a short summary.
func printA2AEvent(out io.Writer, event a2a.Event) {
	switch e := event.(type) {
	case *a2a.Message:
		fmt.Fprintf(out, "message: %s\n", messageText(e))
	case *a2a.Task:
		fmt.Fprintf(out, "task %s: %s\n", e.ID, e.Status.State)
	case *a2a.TaskStatusUpdateEvent:
		line := fmt.Sprintf("status %s: %s", e.TaskID, e.Status.State)
		if e.Status.Message != nil {
			if text := messageText(e.Status.Message); text != "" {
				line += ": " + text
			}
		}
		fmt.Fprintln(out, line)
	case *a2a.TaskArtifactUpdateEvent:
		// Artifact is a pointer and the field is what a peer sent, so a malformed
		// response can leave it nil; report the event rather than panicking on it.
		if e.Artifact == nil {
			fmt.Fprintf(out, "artifact %s: <missing>\n", e.TaskID)
			return
		}
		name := e.Artifact.Name
		if name == "" {
			name = string(e.Artifact.ID)
		}
		line := fmt.Sprintf("artifact %s: %s", e.TaskID, name)
		// An artifact's payload is its parts, and an agent may report a failure
		// there (a stack trace as text, or a structured error as a data part), so
		// they are printed rather than summarized away by name alone.
		if content := partsText(e.Artifact.Parts); content != "" {
			line += ": " + content
		}
		fmt.Fprintln(out, line)
	default:
		fmt.Fprintf(out, "event: %T\n", event)
	}
}

// messageText renders a message's parts as a single line.
func messageText(m *a2a.Message) string {
	return partsText(m.Parts)
}

// partsText renders a sequence of content parts as a single line, skipping any
// that render to nothing. Shared by messages and artifacts, which both carry
// their payload as parts and should print it the same way.
func partsText(parts a2a.ContentParts) string {
	var rendered []string
	for _, p := range parts {
		if p == nil {
			continue
		}
		if text := partText(p); text != "" {
			rendered = append(rendered, text)
		}
	}
	return strings.Join(rendered, " ")
}

// partText renders a part as a single line.
//
// Part.Content is a discriminated union. Text is printable as-is; raw bytes and
// file URLs are summarized rather than dumped. Structured data is JSON-encoded
// rather than skipped: an agent reporting a failure often does so as a data part
// (e.g. {"error": ...}), and dropping it printed nothing at all.
//
// It returns "" only for a part whose content is nil or of a type this build of
// the a2a package does not know, so callers can treat "" as "nothing to show".
func partText(p *a2a.Part) string {
	switch c := p.Content.(type) {
	case a2a.Text:
		return string(c)
	case a2a.URL:
		return fmt.Sprintf("[file %s]", string(c))
	case a2a.Raw:
		name := p.Filename
		if name == "" {
			name = "attachment"
		}
		return fmt.Sprintf("[%s %d bytes]", name, len(c))
	case a2a.Data:
		return dataText(c.Value)
	default:
		return ""
	}
}

// dataText renders a data part's value as compact single-line JSON.
//
// Marshaling can fail on a value JSON cannot represent (a NaN float, a channel),
// and a part that fails to encode is exactly the kind of thing worth seeing, so
// the failure is reported inline rather than swallowed into "".
func dataText(v any) string {
	if v == nil {
		return "[data]"
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("[unprintable data: %v]", err)
	}
	return string(encoded)
}

func init() {
	a2aCmd := newGroup("a2a", "Interact with A2A agents")

	f := a2aSendCmd.Flags()
	f.StringVar(&a2aSendArgs.transport, "transport", defaultA2ATransport,
		"protocol binding to use: jsonrpc, grpc, or http+json")
	f.StringVar(&a2aSendArgs.address, "address", "", "URL of the A2A agent (required)")
	f.BoolVar(&a2aSendArgs.withAuthorization, "with-authorization", false,
		"attach the context's bearer token as an Authorization header")
	f.StringVar(&a2aSendArgs.message, "message", "", "message text to send (required)")

	a2aCmd.AddCommand(a2aSendCmd)
	rootCmd.AddCommand(a2aCmd)
}
