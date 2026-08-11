// Package inprocess answers rossoctl API requests from a serve handler running
// inside this process, so CLI commands can target a local cortex without a
// `rossoctl cortex serve` daemon listening on a port.
//
// This is an http.RoundTripper rather than a second implementation of
// rossoctlclient.Rossoctl. apiclient.Client stays the one client and only its
// transport changes, so a request answered here goes through the same URL
// construction, the same status handling, and the same JSON decoding as one that
// crossed a socket. The two paths cannot disagree about anything above the
// transport, which is the property a second implementation would give up.
//
// What makes this sound is that the serve package holds no state between
// requests: every handler reads ~/.config/rossoctl/namespaces afresh, so a
// running daemon is a pure function of the filesystem. Serving those same
// handlers here produces the same answers, including for the routes that answer
// 500 UNIMPLEMENTED — this package deliberately reproduces those rather than
// improving on them.
package inprocess

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/instances"
	"github.com/rossoctl/rossoctl-cli/internal/serve"
)

// listenAddr is the address handed to serve.New. Nothing is ever bound to it:
// serve.New only validates the address and builds a mux, and binding happens in
// Listen, which this package never calls.
//
// A real-looking loopback address is used rather than a sentinel so the address
// validation serve.New performs here is the same validation the daemon performs,
// and port 0 makes the "nothing is listening" intent explicit to a reader.
const listenAddr = "localhost:0"

// Transport answers HTTP requests from Handler without a network round trip.
//
// It is safe for concurrent use by multiple goroutines: Handler is not mutated
// after construction, and the handler serve.New builds is an http.ServeMux,
// which is itself safe for concurrent use.
type Transport struct {
	// Handler answers every request. New never leaves this nil; a zero
	// Transport reports an error from RoundTrip rather than panicking.
	Handler http.Handler
}

// Ensure Transport satisfies the interface it exists to implement.
var _ http.RoundTripper = (*Transport)(nil)

// RoundTrip serves req with t.Handler and returns the recorded response.
//
// The http.RoundTripper contract is honored on three points that are easy to get
// wrong. req.Body is closed on every path, including the error returns. req
// itself is never modified: the handler is given a clone, which matters because
// ServeMux attaches path values to the request it dispatches and serve's detail
// handlers read them. And the returned response always carries a non-nil,
// closable Body.
//
// A non-2xx response is not an error. RoundTrip returns a non-nil error only for
// a request it cannot serve at all — a nil handler or a request with no URL. A
// 500 UNIMPLEMENTED from serve is a response, and flattening it into a transport
// error would make this path report a different kind of failure than the daemon
// does for the same route; both must reach the caller as the same
// *apiclient.StatusError.
//
// A handler panic is deliberately not recovered. net/http recovers panics per
// connection because one client must not take down a server shared with others.
// Here the server and the client are the same process running one command, so a
// panic is a bug in this binary and should surface as a stack trace rather than
// as a synthesized 500 that hides it.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Deferred first so it also covers the two error returns below, which the
	// contract requires just as much as the success path.
	if req.Body != nil {
		defer func() { _ = req.Body.Close() }()
	}
	if t.Handler == nil {
		return nil, fmt.Errorf("inprocess: transport has no handler")
	}
	if req.URL == nil {
		return nil, fmt.Errorf("inprocess: request has no URL")
	}

	// Serve a clone rather than req itself. "Should not modify the request"
	// covers what the handler does to it too: ServeMux sets path values on the
	// request it dispatches, and Clone's deep copy of the header map keeps a
	// handler that sets a header from writing through to the caller's copy.
	//
	// The caller's context is carried over, so cancelling the command also
	// cancels the one outbound request serve makes (the agent-card fetch).
	r := req.Clone(req.Context())

	// Shape the request the way arriving over a socket would have. ServeMux
	// routes on URL.Path, so RequestURI is belt-and-braces, but a handler that
	// reads it should see what it would see in the daemon. A client-side request
	// leaves RequestURI empty and may leave Body nil, where a server-side one
	// carries http.NoBody.
	r.RequestURI = req.URL.RequestURI()
	if r.RemoteAddr == "" {
		r.RemoteAddr = "127.0.0.1:0"
	}
	if r.Body == nil {
		r.Body = http.NoBody
	}

	// httptest.NewRecorder is the reference ResponseWriter capture, and Result
	// handles several details a hand-rolled recorder gets wrong on the first
	// try: defaulting the status to 200 when a handler never calls WriteHeader,
	// snapshotting headers as they were at first write, deriving ContentLength
	// from the buffer only when the handler did not set it, and wrapping the body
	// so the caller can Close it. It is standard library, so this costs no
	// dependency — only the mild oddity of a package named "test" appearing
	// outside one.
	rec := httptest.NewRecorder()
	t.Handler.ServeHTTP(rec, r)

	resp := rec.Result()
	// http.Client sets this on the response it returns, but setting it here
	// keeps the response self-describing for anything that inspects it earlier.
	resp.Request = req
	return resp, nil
}

// New returns an http.Client that answers ctx's API requests from a serve
// handler in this process.
//
// The handler serves the same routes `rossoctl cortex serve` serves, reading the
// same instances directory on every request, so it answers what a daemon at
// ctx.Server would answer — including 500 UNIMPLEMENTED for the routes serve
// does not implement.
//
// ctx.Server is not dialed, but it is not ignored either: its path is where the
// handler mounts, so the URLs apiclient builds from it and the routes serving
// them cannot drift apart. Its host and port are unused here, and remain
// meaningful to `rossoctl ui open`, which opens them in a browser.
func New(ctx *config.Context) (*http.Client, error) {
	path, err := mountPath(ctx.Server)
	if err != nil {
		return nil, err
	}

	// The namespaces that exist are the ones `authbridge exec` has written
	// records into, which is the same set the agent and tool listings draw from.
	// Deriving GET /namespaces from that keeps `namespaces list` and
	// `agents list` consistent with each other, where the daemon's --namespaces
	// flag can advertise a namespace that holds nothing. A machine where nothing
	// has ever run reports none, which serve renders as an empty list.
	//
	// This is read once per client rather than per request. The daemon fixes its
	// list once at startup, so both are "fixed for the life of the server" — and
	// here that life is a single command.
	namespaces, err := instances.Namespaces()
	if err != nil {
		return nil, fmt.Errorf("listing local namespaces: %w", err)
	}

	srv, err := serve.New(listenAddr, path, namespaces)
	if err != nil {
		return nil, err
	}

	// No Timeout. The only I/O any handler performs is serve's agent-card fetch,
	// which imposes its own deadline; everything else is a filesystem read. A
	// timeout here would be a second, redundant deadline over local work.
	return &http.Client{Transport: &Transport{Handler: srv.Handler()}}, nil
}

// mountPath returns the path a serve handler must mount at to answer the URLs
// apiclient builds from serverURI.
//
// The two have to agree by construction rather than by coincidence: apiclient
// resolves every request path against its base URL as a directory, so a server
// of http://localhost:9097/api/v1/ yields requests at /api/v1/agents, and a
// handler mounted anywhere else would answer all of them with 404. Taking the
// mount path from the same string apiclient takes its base URL from leaves one
// source for the prefix instead of two that must be kept in step.
//
// The trailing slash is appended before parsing for the same reason apiclient
// appends it: without one, url.Parse still reports the path, but the base would
// resolve relative references against the parent, so mirroring the client's own
// normalization is what keeps the two in agreement. serve.New normalizes the
// result further, so a leading slash, a trailing slash, and the root all work.
//
// Host and port are ignored: nothing is bound, so there is no address to match.
func mountPath(serverURI string) (string, error) {
	if serverURI == "" {
		// Worded as apiclient words it for an empty base URL, so an
		// unconfigured context fails the same way whichever transport it uses.
		return "", fmt.Errorf("server URI is empty")
	}
	if !strings.HasSuffix(serverURI, "/") {
		serverURI += "/"
	}
	u, err := url.Parse(serverURI)
	if err != nil {
		return "", fmt.Errorf("invalid server URI %q: %w", serverURI, err)
	}
	return u.Path, nil
}
