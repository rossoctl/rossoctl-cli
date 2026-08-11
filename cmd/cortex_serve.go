package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/serve"
)

// defaultServeAddress is the listen address and mount path used when --address
// is not given. The path mirrors the real backend's /api/v1 prefix so a UI
// pointed here needs no other adjustment.
//
// The port deliberately avoids 9093 and 9094, which `authbridge exec` publishes
// for the AuthBridge admin and session APIs — a default that collided with them
// would fail, or worse reach the wrong server, whenever both were in use.
const defaultServeAddress = "localhost:9097/api/v1"

// defaultServeNamespaces is the namespace list served by GET /namespaces when
// --namespaces is not given.
const defaultServeNamespaces = "team1,team2"

// Bound to `cortex serve`'s flags.
var (
	serveAddress    string
	serveNamespaces string
)

var cortexServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve the rossoctl backend API locally",
	Long: `Serve the rossoctl backend API on a local port.

Run this to point a web UI at a local cortex. rossoctl's own commands do not
need it: a context named "cortex" is answered by these same handlers inside the
command's own process, so "agents list" works with nothing listening. Use
"rossoctl config create-context --name cortex" for that, and this command when
something other than rossoctl has to reach the API over HTTP.

The server exposes the same set of operations as the backend's published
OpenAPI document, so a UI pointed at it finds every endpoint it expects. Most
are placeholders that answer 500 UNIMPLEMENTED. Nine are real:

  GET /auth/config              reports authentication as disabled
  GET /namespaces               reports --namespaces
  GET /agents                   lists the locally running a2a instances
  GET /tools                    lists the locally running mcp instances
  GET /agents/{ns}/{name}       reports one instance in detail
  GET /agents/{ns}/{name}/route-status
                                reports that an existing instance has a route
  GET /chat/{ns}/{name}/agent-card
                                proxies the agent's own A2A card
  GET /health                   reports {"status":"healthy"}
  GET /ready                    reports {"status":"ready"}

The instance endpoints read the records "authbridge exec" writes under
~/.config/rossoctl/namespaces/<namespace>, once per request, so an instance
that starts or stops while the server runs is reflected without a restart.

Every namespace directory is read, and each instance is reported in the namespace
it was recorded in — which is independent of --namespaces. That flag says what
namespaces a UI may offer; an instance's namespace says where it actually is, so
an instance in a namespace this server does not advertise is still listed rather
than hidden.

--address takes a "host:port/path" string. The path is where the API is
mounted, so the default localhost:9097/api/v1 serves the auth config at
http://localhost:9097/api/v1/auth/config. Omitting the path mounts the API at
the root. The /health and /ready probes are served both at the root and under
the mount path.

--namespaces takes a comma-separated list, served in order.

The address printed on startup is the one actually bound, which can differ from
the one requested: a hostname resolving to both IPv4 and IPv6 is bound at only
one of them, and port 0 is assigned a real port by the kernel.

The server runs until interrupted.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		addr, path, err := serve.SplitAddress(serveAddress)
		if err != nil {
			return err
		}

		srv, err := serve.New(addr, path, serve.SplitNamespaces(serveNamespaces))
		if err != nil {
			return err
		}

		// Bind before reporting: the listener's address is concrete, whereas
		// the requested one may name a hostname that resolves to more than one
		// address (only the first of which gets bound) or port 0. Printing the
		// request instead can advertise a URL that reaches a different process.
		ln, err := srv.Listen()
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Serving the rossoctl API on http://%s%s\n", ln.Addr(), srv.Path())
		fmt.Fprintf(out, "GET /health and GET /ready report the server is up, "+
			"GET /auth/config reports authentication as disabled, "+
			"GET /namespaces reports %s, and GET /agents, GET /tools and "+
			"GET /agents/{namespace}/{name} report the locally running instances; "+
			"other operations return 500 UNIMPLEMENTED.\n",
			strings.Join(srv.Namespaces(), ", "))

		if err := srv.Serve(ln); err != nil {
			return fmt.Errorf("serving on %s: %w", ln.Addr(), err)
		}
		return nil
	},
}

func init() {
	f := cortexServeCmd.Flags()
	f.StringVar(&serveAddress, "address", defaultServeAddress,
		`address to serve on, as "host:port/path"`)
	f.StringVar(&serveNamespaces, "namespaces", defaultServeNamespaces,
		"comma-separated namespaces for GET /namespaces to report")
}
