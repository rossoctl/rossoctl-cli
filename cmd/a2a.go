package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
authenticating proxy. Note the token is sent to whatever --address names, so
point it only at agents you would hand that credential to.

With --verbose each request and its status are reported on stderr, leaving the
streamed events alone on stdout.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if a2aSendArgs.address == "" {
			return fmt.Errorf("--address is required")
		}
		if a2aSendArgs.message == "" {
			return fmt.Errorf("--message is required")
		}

		return streamA2AMessage(cmd, a2aSendOptions{
			address:           a2aSendArgs.address,
			transport:         a2aSendArgs.transport,
			message:           a2aSendArgs.message,
			withAuthorization: a2aSendArgs.withAuthorization,
		})
	},
}

// a2aSendOptions is one A2A send: who to talk to, how, and what to say.
type a2aSendOptions struct {
	address           string
	transport         string
	message           string
	withAuthorization bool
}

// a2aLogger is an http.RoundTripper that logs each A2A request and its status,
// and remembers the last status seen.
//
// This lives at the HTTP layer rather than in an a2aclient.CallInterceptor,
// which is the more obvious home, for two reasons found by observing the client:
//
//   - The interceptor's After hook runs once per streamed event, so logging a
//     response there would print a line per event and duplicate the event
//     output the command already prints.
//   - a2aclient's transports discard the HTTP status: a non-200 becomes
//     fmt.Errorf("unexpected HTTP status: %s"), an untyped error carrying no
//     code. A 401 is therefore not detectable from the error without matching
//     that string, which is the library's own wording and free to change.
//     Here the status is read before the library flattens it.
//
// It wraps whatever RoundTripper it is given, so the caller decides the
// underlying transport.
type a2aLogger struct {
	rt http.RoundTripper

	// logf, if non-nil, is called with one line per request and per response.
	logf func(format string, args ...any)

	// status is the HTTP status of the last response received, or 0 if no
	// response ever arrived (a dial failure, say). It is read after the request
	// completes to decide which hint to offer, so no locking is needed: the
	// commands here issue one A2A call at a time and read this only once the
	// stream has ended.
	status int
}

func (l *a2aLogger) log(format string, args ...any) {
	if l.logf != nil {
		l.logf(format, args...)
	}
}

func (l *a2aLogger) RoundTrip(req *http.Request) (*http.Response, error) {
	// The URL is logged rather than the body: a request body is the A2A payload,
	// which carries the message text and can be large, and --verbose is about
	// which calls were made rather than their contents.
	l.log("%s %s", req.Method, req.URL)
	start := time.Now()

	rt := l.rt
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := rt.RoundTrip(req)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		// Deliberately does not set status: there was no response, and leaving it
		// 0 keeps "never got an answer" distinct from any real status.
		l.log("%s %s failed after %s: %v", req.Method, req.URL, elapsed, err)
		return resp, err
	}
	l.status = resp.StatusCode
	l.log("%s %s -> %s (%s)", req.Method, req.URL, resp.Status, elapsed)
	return resp, nil
}

// a2aUnauthorizedHint returns the advice to offer when an A2A call failed with
// 401, or "" when the failure was anything else.
//
// The two cases need different advice because the user's next step differs. With
// no token sent there is a credential to add; with one sent and still rejected
// the token itself is the thing to inspect — and a token predating an agent may
// simply lack its scope, which no amount of retrying fixes but a fresh login
// does.
func a2aUnauthorizedHint(status int, withAuthorization bool) string {
	if status != http.StatusUnauthorized {
		return ""
	}
	if !withAuthorization {
		return "Hint: the agent requires credentials and none were sent. " +
			"Retry with --with-authorization to attach the context's bearer token."
	}
	return "Hint: the agent rejected the bearer token that was sent. " +
		"Run `rossoctl auth status` to inspect it — and if the agent was created after you signed in, " +
		"run `rossoctl login` again to pick up the scopes for it."
}

// streamA2AMessage sends one message to an A2A agent and prints the events it
// streams back.
//
// This is the whole of `a2a send` minus flag validation, factored out so
// `agents chat` is the same code rather than a copy of it: the two commands
// differ only in how they learn the agent's address, and a second
// implementation would let their transport handling, authorization, and event
// rendering drift apart.
//
// The caller is responsible for rejecting an empty address and message, since
// what makes them missing differs — a flag the user omitted, or an agent card
// that carried no URL — and so does the error worth reporting.
func streamA2AMessage(cmd *cobra.Command, opts a2aSendOptions) error {
	protocol, err := a2aTransport(opts.transport)
	if err != nil {
		return err
	}

	var factoryOpts []a2aclient.FactoryOption
	if opts.withAuthorization {
		_, token, err := resolveServer()
		if err != nil {
			return err
		}
		if token == "" {
			return fmt.Errorf("--with-authorization given but the current context has no bearer token; run `rossoctl login` to sign in")
		}
		factoryOpts = append(factoryOpts, a2aclient.WithCallInterceptors(&bearerInterceptor{token: token}))
	}

	// Route the HTTP-based bindings through a2aLogger, which supplies both the
	// --verbose request lines and the status a 401 hint needs. The client's own
	// interceptors cannot do either (see a2aLogger).
	logger := &a2aLogger{}
	if verbose {
		errOut := cmd.ErrOrStderr()
		logger.logf = func(format string, args ...any) {
			fmt.Fprintf(errOut, format+"\n", args...)
		}
	}
	httpClient := &http.Client{Transport: logger}
	factoryOpts = append(factoryOpts,
		a2aclient.WithTransport(a2a.TransportProtocolJSONRPC,
			a2aclient.TransportFactoryFn(func(_ context.Context, _ *a2a.AgentCard, iface *a2a.AgentInterface) (a2aclient.Transport, error) {
				return a2aclient.NewJSONRPCTransport(iface.URL, httpClient), nil
			})),
		a2aclient.WithTransport(a2a.TransportProtocolHTTPJSON,
			a2aclient.TransportFactoryFn(func(_ context.Context, _ *a2a.AgentCard, iface *a2a.AgentInterface) (a2aclient.Transport, error) {
				u, err := url.Parse(iface.URL)
				if err != nil {
					return nil, fmt.Errorf("parsing agent URL %q: %w", iface.URL, err)
				}
				return a2aclient.NewRESTTransport(u, httpClient), nil
			})),
	)

	ctx := cmd.Context()
	iface := a2a.NewAgentInterface(opts.address, protocol)
	client, err := a2aclient.NewFromEndpoints(ctx, []*a2a.AgentInterface{iface}, factoryOpts...)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", opts.address, err)
	}
	defer func() { _ = client.Destroy() }()

	req := &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(opts.message)),
	}

	out := cmd.OutOrStdout()
	eventCount := 0
	for event, err := range client.SendStreamingMessage(ctx, req) {
		// The iterator reports a failed call by yielding an error; stop at
		// the first one rather than continuing to print, since the stream
		// is not resumable.
		if err != nil {
			// A 401 says nothing about what to do next, so the remedy is named
			// here. The hint goes to stderr and the error is still returned
			// unchanged, so it stays out of piped output and the exit status is
			// unaffected.
			if hint := a2aUnauthorizedHint(logger.status, opts.withAuthorization); hint != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), hint)
			}
			return err
		}
		eventCount++
		printA2AEvent(out, event)
	}
	if eventCount == 0 {
		return fmt.Errorf("agent returned no A2A events")
	}
	return nil
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
